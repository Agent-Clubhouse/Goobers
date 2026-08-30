package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/stateclient"
)

// remediationnoopguard.go is pr-remediation's no-op loop breaker: the record
// that stops the lane burning a full agentic remediation cycle re-attempting a
// PR whose previous attempt already concluded there was nothing to do.
//
// SINCE Goobers#3989 IT IS ENTIRELY PLANE-ROUTED, and that is what lets
// `gather-pr-context` run in a stage pod at all. Three seams, because this
// guard earned executor.StageRequiresInstanceRoot three separate ways:
//
//  1. C2 scheduler state. The record used to be a map inside one fixed
//     SchedulerDir()/pr-remediation-noop.json, which is not a
//     stateclient.ValidKey shape, so the plane could not serve it. It is now
//     one key per (gaggle, PR) — stateclient.PRRemediationNoopKey over
//     remediationNoopStateKey's digest — reached through openStageStateStore.
//  2. C1 claims. The terminal-cleanup writer resolved the run's PR claim with
//     localscheduler.OpenClaimLedger under its own withClaimLock; it now goes
//     through the claims seam (stageClaimLedgerForRun / Locked), so a pod
//     reaches the daemon's ledger instead of a claims.json it does not have.
//  3. C4 journal read. The same writer read the terminal run's journal with
//     journal.OpenRead over an instance.Layout.FindRunDir path — the un-seamed
//     read that keeps `issue-close-out` refused. It now goes through
//     stageRunJournal, the seam applyverdict/respondtofindings/
//     resolvereviewthreads already use.
//
// MUTUAL EXCLUSION IS UNCHANGED. Every no-op key falls through
// schedulerStateLock's default arm, which is claims.lock — the very lock this
// guard took before the plane existed, and the lock the daemon holds when it
// serves a pod's compare-and-swap. So a pod-executed gather-pr-context and a
// daemon-driven terminal cleanup contend on ONE lock in ONE atomicity domain,
// exactly as two subprocesses did.
//
// FAIL CLOSED, everywhere. A partial plane configuration (an endpoint with no
// bearer, or no gaggle/run identity) is a refusal from stateclient.Select /
// claimsclient.Select / journalclient.Select, never a silent fall-through to a
// local file a pod does not have — because an absent record reads as "no prior
// no-op", and that is precisely the silent fail-open this issue exists to
// remove. A decode failure is likewise an error the stage surfaces, never an
// empty record.

const (
	// legacyRemediationNoopStateFile is the pre-#3989 aggregate file: one
	// fixed name holding every PR's record in a map. It is read once, at
	// daemon start, so an instance upgrading across this change does not lose
	// the in-flight records that are actively suppressing a re-attempt — see
	// migrateLegacyRemediationNoopState.
	legacyRemediationNoopStateFile = "pr-remediation-noop.json"
	// remediationNoopSchema versions the per-PR document.
	remediationNoopSchema = "goobers.dev/pr-remediation-noop/v1"
	remediationNoopLimit  = 2
	// remediationNoopLockOperation labels the claims-lock critical section a
	// no-op record's read-modify-write takes on the file backend, alongside
	// providercmd.go's claim operations.
	remediationNoopLockOperation = "pr-remediation-noop.update"
)

type remediationNoopSignature struct {
	HeadSHA    string `json:"headSha"`
	Causes     string `json:"causes,omitempty"`
	DiffDigest string `json:"diffDigest,omitempty"`
}

type remediationNoopRecord struct {
	remediationNoopSignature
	Attempts  int    `json:"attempts"`
	LastRunID string `json:"lastRunId"`
	Parked    bool   `json:"parked,omitempty"`
}

// empty reports whether the record is the zero record — what an absent key and
// a cleared key both read as. `delete(state.Records, key)` was the pre-plane
// spelling of this; on a keyed namespace with no delete primitive, clearing
// writes the zero document instead, which the map lookup's zero value made
// indistinguishable anyway.
func (r remediationNoopRecord) empty() bool { return r == remediationNoopRecord{} }

// remediationNoopDocument is one PR's record as it lives at its own
// scheduler-state key. It carries the record key it was written for so a
// mis-keyed document is caught rather than acted on — the same integrity
// posture backlogHealthCursor takes with its coordinates.
type remediationNoopDocument struct {
	Schema string                `json:"schema"`
	Key    string                `json:"key"`
	Record remediationNoopRecord `json:"record"`
}

// legacyRemediationNoopState is the aggregate document's shape, retained only
// for the one-time migration.
type legacyRemediationNoopState struct {
	Records map[string]remediationNoopRecord `json:"records"`
}

type remediationNoopUpdate struct {
	remediationNoopSignature
	implementSucceeded bool
}

// remediationNoopKey is the record's logical identity: the gaggle and the PR,
// unchanged from the pre-plane map key so a migrated record keeps meaning the
// same PR.
func remediationNoopKey(gaggle string, number int) string {
	return gaggle + "/" + pullRequestClaimKey(number)
}

// remediationNoopStateKey is the scheduler-state key holding one PR's record.
// A pure function of remediationNoopKey, which is already the record's
// canonical, unambiguous identity — a gaggle is a single path element so it
// cannot contain the "/" that separates it from the PR — so the SAME PR
// reaches the same key whether the caller is the daemon's terminal cleanup or
// a gather-pr-context stage pod talking to the scheduler-state plane.
func remediationNoopStateKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return stateclient.PRRemediationNoopKey(fmt.Sprintf("%x", sum))
}

func normalizeRemediationCauses(raw string) string {
	parts := splitLabelList(raw)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// decodeRemediationNoopRecord reads one PR's record out of a scheduler-state
// value. An absent key is the zero record — the overwhelmingly common
// first-run state — but a value that is present and unreadable is an ERROR,
// never a zero record: "unreadable" must not be indistinguishable from "no
// prior no-op", or the guard fails open on corruption exactly as it did in a
// pod.
func decodeRemediationNoopRecord(value stateclient.Value, key string) (remediationNoopRecord, error) {
	if !value.Exists() {
		return remediationNoopRecord{}, nil
	}
	var doc remediationNoopDocument
	if err := json.Unmarshal(value.Data, &doc); err != nil {
		return remediationNoopRecord{}, fmt.Errorf("decode remediation no-op state: %w", err)
	}
	if doc.Schema != remediationNoopSchema {
		return remediationNoopRecord{}, fmt.Errorf(
			"decode remediation no-op state: unsupported schema %q, want %q", doc.Schema, remediationNoopSchema)
	}
	if doc.Key != key {
		return remediationNoopRecord{}, fmt.Errorf(
			"decode remediation no-op state: record is keyed to %q, not %q", doc.Key, key)
	}
	return doc.Record, nil
}

func encodeRemediationNoopRecord(key string, record remediationNoopRecord) ([]byte, error) {
	return json.Marshal(remediationNoopDocument{
		Schema: remediationNoopSchema,
		Key:    key,
		Record: record,
	})
}

// readRemediationNoopRecord is one PR's record over a store.
func readRemediationNoopRecord(ctx context.Context, store stateclient.Store, key string) (remediationNoopRecord, error) {
	value, err := store.Get(ctx, remediationNoopStateKey(key))
	if err != nil {
		return remediationNoopRecord{}, fmt.Errorf("read remediation no-op state: %w", err)
	}
	return decodeRemediationNoopRecord(value, key)
}

// updateRemediationNoopRecord is the record's read-modify-write: one lock
// acquisition on the file backend, one compare-and-swap on the plane. fn
// returns write=false to leave the key untouched, and MUST be safe to run more
// than once — the plane re-runs it against the new value when a CAS loses.
func updateRemediationNoopRecord(
	ctx context.Context,
	store stateclient.Store,
	key string,
	fn func(remediationNoopRecord) (remediationNoopRecord, bool, error),
) error {
	return store.Update(ctx, remediationNoopStateKey(key), remediationNoopLockOperation,
		func(value stateclient.Value) ([]byte, bool, error) {
			current, err := decodeRemediationNoopRecord(value, key)
			if err != nil {
				return nil, false, err
			}
			next, write, err := fn(current)
			if err != nil || !write {
				return nil, false, err
			}
			data, err := encodeRemediationNoopRecord(key, next)
			if err != nil {
				return nil, false, err
			}
			return data, true, nil
		})
}

// remediationNoopStore is the scheduler-state store a stage-side guard call
// uses: the plane in a pod, the instance's own file under claims.lock on a
// type-1/type-2 host.
func remediationNoopStore(l instance.Layout) (stateclient.Store, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return nil, fmt.Errorf("open remediation no-op state: %w", err)
	}
	return store, nil
}

// heldRemediationNoopStore is the store a guard call inside the claim ledger's
// critical section uses. WHICH store that is depends on whether the caller is
// actually holding a LOCAL lock:
//
//   - Off the claims plane, Locked is one real claims.lock acquisition, so the
//     store must take no lock of its own — claims.lock is not reentrant and a
//     second acquisition would wait on itself until the timeout.
//   - ON the claims plane there is no client-side lock at all (the daemon
//     serializes, DS1), so the store must take its own: the state plane's
//     compare-and-swap when the state plane is configured too, and — for a
//     PARTIAL configuration where only the claims plane is stamped — the
//     locking file backend, which is claims.lock again. Reusing the held,
//     lock-free store there would be a silent lost update, which is the exact
//     failure class this guard exists to prevent.
func heldRemediationNoopStore(l instance.Layout) (stateclient.Store, error) {
	open := openHeldStageStateStore
	if claimsPlaneSelected() {
		open = openStageStateStore
	}
	store, err := open(l)
	if err != nil {
		return nil, fmt.Errorf("open remediation no-op state: %w", err)
	}
	return store, nil
}

// remediationNoopGaggle is the gaggle a stage-side guard call keys its record
// under: the layout's when there is one, else the dispatcher-stamped
// GOOBERS_GAGGLE a pod carries.
func remediationNoopGaggle(l instance.Layout) string {
	if gaggle := l.Gaggle(); gaggle != "" {
		return gaggle
	}
	return providerGaggle()
}

// recordPRRemediationNoop is terminal cleanup's writer: it reads the finished
// run's journal, decides whether the run was a remediation no-op, and folds
// that into the claimed PR's record — all three planes in one call.
func recordPRRemediationNoop(l instance.Layout, runID string) error {
	update, err := preparePRRemediationNoopUpdate(l, runID)
	if err != nil || update == nil {
		return err
	}
	ctx := claimContext()
	ledger, err := stageClaimLedgerForRun(l, l.Gaggle(), runID)
	if err != nil {
		return fmt.Errorf("open claim ledger: %w", err)
	}
	return ledger.Locked(ctx, claimLockOperationPRRelease, func(held claimsclient.Ledger) error {
		entries, err := held.ForRunAll(ctx, runID)
		if err != nil {
			return fmt.Errorf("read run claims: %w", err)
		}
		return recordPRRemediationNoopLocked(ctx, l, entries, runID, *update)
	})
}

// preparePRRemediationNoopUpdate reduces a finished run's journal to the
// no-op signature its PR record should carry, or nil when the run said nothing
// about remediation progress.
//
// The read is the C4 seam, not journal.OpenRead over a FindRunDir path: an
// unreadable journal is an ERROR the caller surfaces rather than an empty
// event list, because a silent zero here is a policy change — it would record
// "no no-op" for a run that actually made none.
func preparePRRemediationNoopUpdate(l instance.Layout, runID string) (*remediationNoopUpdate, error) {
	reader, err := stageRunJournal(l.Root, runID)
	if err != nil {
		return nil, fmt.Errorf("find terminal run journal: %w", err)
	}
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}

	var update remediationNoopUpdate
	implementNoWork := false
	for _, event := range events {
		if event.Type != journal.EventStageFinished {
			continue
		}
		switch event.Stage {
		case "rebase-pr":
			update.HeadSHA = scalarString(event.Outputs["attemptedHeadSha"])
			update.Causes = normalizeRemediationCauses(scalarString(event.Outputs["remediationCauses"]))
		case "implement":
			switch event.Status {
			case string(apiv1.ResultSuccess):
				update.implementSucceeded = true
			case string(apiv1.ResultNoWork):
				implementNoWork = true
			}
		}
	}
	if !update.implementSucceeded && !implementNoWork {
		return nil, nil
	}
	if !update.implementSucceeded && (update.HeadSHA == "" || update.Causes == "") {
		return nil, nil
	}
	return &update, nil
}

// recordPRRemediationNoopLocked folds update into the record of the PR the run
// holds a claim on. entries are the run's ledger entries, already read by the
// caller inside its critical section — the claims seam on the terminal-cleanup
// path, the daemon's own ledger on the claim-recovery path.
func recordPRRemediationNoopLocked(
	ctx context.Context,
	l instance.Layout,
	entries []claimsclient.Entry,
	runID string,
	update remediationNoopUpdate,
) error {
	for _, entry := range entries {
		if !strings.HasPrefix(entry.ItemID, pullRequestClaimPrefix) {
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(entry.ItemID, pullRequestClaimPrefix))
		if err != nil {
			return fmt.Errorf("parse PR claim %q: %w", entry.ItemID, err)
		}
		store, err := heldRemediationNoopStore(l)
		if err != nil {
			return err
		}
		key := remediationNoopKey(entry.Gaggle, number)
		if update.implementSucceeded {
			return clearRemediationNoopState(ctx, store, key)
		}
		return updateRemediationNoopState(ctx, store, key, update.remediationNoopSignature, runID)
	}
	return nil
}

func scalarString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}

// updateRemediationNoopState counts one more consecutive no-op for key under
// signature. A run that already counted does not count twice.
func updateRemediationNoopState(
	ctx context.Context,
	store stateclient.Store,
	key string,
	signature remediationNoopSignature,
	runID string,
) error {
	return updateRemediationNoopRecord(ctx, store, key,
		func(record remediationNoopRecord) (remediationNoopRecord, bool, error) {
			if record.remediationNoopSignature != signature {
				record = remediationNoopRecord{remediationNoopSignature: signature}
			}
			if record.LastRunID == runID {
				return record, false, nil
			}
			record.Attempts++
			record.LastRunID = runID
			return record, true, nil
		})
}

// recordGatherPRContextDigestNoop is gather-pr-context's own writer: it counts
// this run's unchanged-digest observation for the PR and reports the resulting
// record, plus whether an operator cleared a parked escalation (which resets
// the guard for a fresh attempt).
func recordGatherPRContextDigestNoop(
	l instance.Layout,
	number int,
	signature remediationNoopSignature,
	runID string,
	escalatedLabelPresent bool,
) (remediationNoopRecord, bool, error) {
	if runID == "" {
		return remediationNoopRecord{}, false, fmt.Errorf("GOOBERS_RUN_ID is required to record an unchanged remediation digest")
	}
	store, err := remediationNoopStore(l)
	if err != nil {
		return remediationNoopRecord{}, false, err
	}
	var recorded remediationNoopRecord
	var operatorReset bool
	key := remediationNoopKey(remediationNoopGaggle(l), number)
	err = updateRemediationNoopRecord(stateContext(), store, key,
		func(record remediationNoopRecord) (remediationNoopRecord, bool, error) {
			// fn may run more than once on the plane (a lost CAS is retried),
			// so both reported values are assigned from THIS attempt's view,
			// never accumulated across attempts.
			recorded = remediationNoopRecord{}
			operatorReset = false
			if record.remediationNoopSignature == signature && record.Parked && !escalatedLabelPresent {
				operatorReset = true
				return remediationNoopRecord{}, true, nil
			}
			if record.remediationNoopSignature != signature {
				record = remediationNoopRecord{remediationNoopSignature: signature}
			}
			write := false
			if record.LastRunID != runID {
				record.Attempts++
				record.LastRunID = runID
				write = true
			}
			recorded = record
			return record, write, nil
		})
	if err != nil {
		return remediationNoopRecord{}, false, err
	}
	return recorded, operatorReset, nil
}

// clearRemediationNoopState forgets key's record. An already-absent record is
// left untouched rather than rewritten as the zero document, so a clear that
// changes nothing costs no write on either backend.
func clearRemediationNoopState(ctx context.Context, store stateclient.Store, key string) error {
	return updateRemediationNoopRecord(ctx, store, key,
		func(record remediationNoopRecord) (remediationNoopRecord, bool, error) {
			if record.empty() {
				return record, false, nil
			}
			return remediationNoopRecord{}, true, nil
		})
}

// remediationNoopRecordForSignature reads the PR's record when it still
// matches signature, and forgets it when it does not — a changed head or a
// changed cause set means the prior no-op streak says nothing about this
// attempt.
func remediationNoopRecordForSignature(
	l instance.Layout,
	number int,
	signature remediationNoopSignature,
) (remediationNoopRecord, error) {
	store, err := remediationNoopStore(l)
	if err != nil {
		return remediationNoopRecord{}, err
	}
	var matched remediationNoopRecord
	key := remediationNoopKey(remediationNoopGaggle(l), number)
	err = updateRemediationNoopRecord(stateContext(), store, key,
		func(record remediationNoopRecord) (remediationNoopRecord, bool, error) {
			matched = remediationNoopRecord{}
			if record.empty() {
				return record, false, nil
			}
			if record.remediationNoopSignature != signature {
				return remediationNoopRecord{}, true, nil
			}
			matched = record
			return record, false, nil
		})
	if err != nil {
		return remediationNoopRecord{}, err
	}
	return matched, nil
}

// markRemediationNoopParked records that the PR was visibly parked, which is
// what makes an operator clearing the escalation label a recognisable reset
// rather than another silent no-op.
func markRemediationNoopParked(l instance.Layout, key string) error {
	store, err := remediationNoopStore(l)
	if err != nil {
		return err
	}
	return updateRemediationNoopRecord(stateContext(), store, key,
		func(record remediationNoopRecord) (remediationNoopRecord, bool, error) {
			if record.empty() || record.Parked {
				return record, false, nil
			}
			record.Parked = true
			return record, true, nil
		})
}

// clearRemediationNoopRecord is the stage-side clear.
func clearRemediationNoopRecord(l instance.Layout, key string) error {
	store, err := remediationNoopStore(l)
	if err != nil {
		return err
	}
	return clearRemediationNoopState(stateContext(), store, key)
}

// migrateLegacyRemediationNoopState folds the pre-#3989 aggregate file into
// the per-PR keys, once, at daemon start.
//
// It matters because the record is a LOOP BREAKER: an instance that upgraded
// across the keyed shape without this would read every in-flight record as
// absent, and every PR currently being suppressed would get one more full
// agentic remediation cycle spent proving again that there is nothing to do.
// The daemon is the right place for it — it is the only party with both the
// legacy file and the claims lock, and doing it here means a pod's very first
// plane read already sees the migrated record.
//
// Idempotent, and safe to interrupt: the legacy file is removed only after
// every record it held has been written to its own key, and a record whose key
// already exists is left alone (the keyed value is by definition newer).
func migrateLegacyRemediationNoopState(l instance.Layout) error {
	schedulerDir := l.SchedulerDir()
	legacyPath := filepath.Join(schedulerDir, legacyRemediationNoopStateFile)
	if _, err := os.Stat(legacyPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect legacy remediation no-op state: %w", err)
	}
	return withClaimLock(filepath.Join(schedulerDir, claimLockFileName), remediationNoopLockOperation, func() error {
		data, err := os.ReadFile(legacyPath)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read legacy remediation no-op state: %w", err)
		}
		var legacy legacyRemediationNoopState
		if err := json.Unmarshal(data, &legacy); err != nil {
			return fmt.Errorf("decode legacy remediation no-op state: %w", err)
		}
		// The file backend WITHOUT a lock: claims.lock is held above and is
		// not reentrant. Deliberately not the stage seam either — the daemon
		// is the writer here, not a client, and the legacy file only ever
		// exists on the instance's own disk.
		store, err := heldStateStore(l)
		if err != nil {
			return err
		}
		ctx := stateContext()
		keys := make([]string, 0, len(legacy.Records))
		for key := range legacy.Records {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			record := legacy.Records[key]
			if record.empty() {
				continue
			}
			stateKey := remediationNoopStateKey(key)
			existing, err := store.Get(ctx, stateKey)
			if err != nil {
				return fmt.Errorf("read migrated remediation no-op state: %w", err)
			}
			if existing.Exists() {
				continue
			}
			encoded, err := encodeRemediationNoopRecord(key, record)
			if err != nil {
				return err
			}
			if _, err := store.Put(ctx, stateKey, encoded, ""); err != nil {
				return fmt.Errorf("write migrated remediation no-op state: %w", err)
			}
		}
		if err := os.Remove(legacyPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove legacy remediation no-op state: %w", err)
		}
		return nil
	})
}
