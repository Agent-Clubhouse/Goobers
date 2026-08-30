package main

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// The backlog-health stage used to rebuild the ready-label transition ledger by
// paginating the repository's ENTIRE issue-event history on every check cycle
// (#3392) — 200+ pages on an aged repo, and the page count only ever grows. On
// a live instance that spent the shared GitHub App installation credential to
// `remaining 0` roughly every three hours, starving claims, label writes, PR
// creation, curation, and merge-review, all of which draw on the same budget: a
// health check that made the instance less healthy.
//
// The ledger is now durable and resumed. A per-(gaggle, provider, repo, label)
// cursor under the instance-wide scheduler dir records the highest provider
// event id already folded in, plus the accumulated transitions themselves —
// accumulation is what makes resumption possible at all, since a currently-ready
// item's "labeled" event may be arbitrarily far back in history and still has to
// resolve its ReadyAt. Steady state therefore reads only the pages holding
// events newer than the cursor.
//
// Since #3948 the cursor is reached through the scheduler-state seam
// (openStageStateStore) rather than through os.ReadFile: the same file, in the
// same place, under a real cross-process lock locally, and over the daemon's
// C2 plane from a stage pod — which is what lets `backlog-health` run in a pod
// at all. Losing the ledger in a pod would not degrade gracefully: an absent
// cursor silently reruns a bounded FULL scan and can defer the ready-pool
// snapshot, which the telemetry rollup reads as a starvation signal.
//
// A full scan runs only when there is no usable cursor: first run, or an
// integrity mismatch (unparsable file, a cursor keyed to a different
// repo/label, a ledger whose contents contradict its own high-water mark, or a
// resumed ledger that cannot explain the live ready pool). Which mode ran, and
// why, is reported in the stage artifact and on stdout.
const backlogHealthCursorSchema = "goobers.dev/backlog-health-cursor/v1"

// Scan modes and the reasons a full scan was chosen, as reported in the
// backlog-health artifact's scan block.
const (
	backlogHealthScanIncremental = "incremental"
	backlogHealthScanFull        = "full"

	backlogHealthScanFirstRun          = "first-run"
	backlogHealthScanIntegrityMismatch = "integrity-mismatch"
	backlogHealthScanLedgerMismatch    = "ledger-does-not-explain-ready-pool"
	backlogHealthScanUnsupported       = "provider-cursor-unsupported"
)

// backlogHealthCursor is the durable resume point plus the accumulated ledger.
type backlogHealthCursor struct {
	Schema     string `json:"schema"`
	Gaggle     string `json:"gaggle,omitempty"`
	Provider   string `json:"provider"`
	Repository string `json:"repository"`
	Label      string `json:"label"`
	// HighWaterEventID is the greatest provider event id already examined —
	// including events that did not match Label, so the next scan never
	// re-reads them either.
	HighWaterEventID int64                               `json:"highWaterEventId"`
	ScannedAt        time.Time                           `json:"scannedAt"`
	Transitions      []providers.WorkItemLabelTransition `json:"transitions"`
}

// backlogHealthScan is the per-run scan provenance embedded in the stage
// artifact. It exists so an operator reading a journal can tell a one-page
// resume from a full-history rescan without inferring it from timing.
type backlogHealthScan struct {
	Mode           string `json:"mode"`
	Reason         string `json:"reason,omitempty"`
	Pages          int    `json:"pages"`
	NewTransitions int    `json:"newTransitions"`
	LedgerSize     int    `json:"ledgerSize"`
	FromEventID    int64  `json:"fromEventId,omitempty"`
	ToEventID      int64  `json:"toEventId,omitempty"`
	Deferred       bool   `json:"deferred,omitempty"`
	DeferReason    string `json:"deferReason,omitempty"`
	QuotaLimit     int    `json:"quotaLimit,omitempty"`
	QuotaRemaining int    `json:"quotaRemaining,omitempty"`
}

// resumable reports whether a failed read of this scan's ledger is worth
// retrying as a full rescan. Only a resumed ledger can be stale; a full scan
// that already read all of history is authoritative.
func (s backlogHealthScan) resumable() bool {
	return s.Mode == backlogHealthScanIncremental
}

// backlogHealthCursorKey is the scheduler-state key addressing this scan's
// ready-transition ledger (#3948). Both halves come from the SAME sanitizer
// instance.Layout.BacklogHealthCursorPath builds the file name with, so the
// plane backend and the file backend can never disagree about which ledger a
// scan means.
func backlogHealthCursorKey(gaggle string, repo providers.RepositoryRef, label string) string {
	return stateclient.BacklogHealthCursorKey(
		instance.SchedulerNameSegment(gaggle),
		instance.BacklogHealthCursorScope(
			string(repo.Provider), backlogHealthCursorRepositoryKey(repo), label))
}

// backlogHealthCursorRepositoryKey is the provider-native repository key the
// cursor file is named for.
func backlogHealthCursorRepositoryKey(repo providers.RepositoryRef) string {
	if repo.Project != "" {
		return repo.Owner + "/" + repo.Project + "/" + repo.Name
	}
	return repo.Owner + "/" + repo.Name
}

// decodeBacklogHealthCursor reads the durable cursor out of one scheduler-state
// value. It never fails the stage: a missing, unreadable, malformed, mis-keyed,
// or self-contradictory cursor simply reports the reason a full scan is
// required, so a corrupted ledger self-heals on the next cycle instead of
// wedging the check.
func decodeBacklogHealthCursor(
	value stateclient.Value,
	gaggle string,
	repo providers.RepositoryRef,
	label string,
) (backlogHealthCursor, string) {
	if !value.Exists() {
		return backlogHealthCursor{}, backlogHealthScanFirstRun
	}
	var cursor backlogHealthCursor
	if err := json.Unmarshal(value.Data, &cursor); err != nil {
		return backlogHealthCursor{}, backlogHealthScanIntegrityMismatch
	}
	if cursor.Schema != backlogHealthCursorSchema ||
		cursor.Gaggle != gaggle ||
		cursor.Provider != string(repo.Provider) ||
		cursor.Repository != backlogHealthCursorRepositoryKey(repo) ||
		cursor.Label != label ||
		cursor.HighWaterEventID <= 0 {
		return backlogHealthCursor{}, backlogHealthScanIntegrityMismatch
	}
	for _, transition := range cursor.Transitions {
		if transition.EventID <= 0 || transition.ItemID == "" ||
			transition.Label != label || transition.OccurredAt.IsZero() ||
			transition.EventID > cursor.HighWaterEventID {
			return backlogHealthCursor{}, backlogHealthScanIntegrityMismatch
		}
	}
	return cursor, ""
}

// encodeBacklogHealthCursor renders the cursor exactly as the pre-plane writer
// did, trailing newline included, so a type-1/type-2 instance's file keeps the
// bytes it had.
func encodeBacklogHealthCursor(cursor backlogHealthCursor) ([]byte, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// mergeLabelTransitions folds a scan's transitions into the durable ledger,
// deduplicating on the provider event id. Overlap is expected — a resumed scan
// re-reads the page its cursor sits on — and event ids make it harmless.
func mergeLabelTransitions(
	ledger, fresh []providers.WorkItemLabelTransition,
) []providers.WorkItemLabelTransition {
	byEvent := make(map[int64]providers.WorkItemLabelTransition, len(ledger)+len(fresh))
	for _, transition := range ledger {
		byEvent[transition.EventID] = transition
	}
	for _, transition := range fresh {
		byEvent[transition.EventID] = transition
	}
	merged := make([]providers.WorkItemLabelTransition, 0, len(byEvent))
	for _, transition := range byEvent {
		merged = append(merged, transition)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].OccurredAt.Equal(merged[j].OccurredAt) {
			return merged[i].EventID < merged[j].EventID
		}
		return merged[i].OccurredAt.Before(merged[j].OccurredAt)
	})
	return merged
}
