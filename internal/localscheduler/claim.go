package localscheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// ClaimEntry is one lease in the claim ledger.
type ClaimEntry struct {
	ItemID    string    `json:"itemId"`
	RunID     string    `json:"runId"`
	Workflow  string    `json:"workflow"`
	ClaimedAt time.Time `json:"claimedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// expired reports whether the lease is no longer live at now.
func (e ClaimEntry) expired(now time.Time) bool { return !e.ExpiresAt.After(now) }

// ClaimLedger is the authoritative, atomic, lease-based source of truth for
// exactly-once backlog-item processing (SCH-020/BL-005). A provider-visible
// marker (#12's providers.ClaimWorkItem) mirrors this ledger for human
// visibility once a local claim succeeds — the ledger never depends on the
// provider layer, and the marker is never the source of truth (§7, SCH-Q5).
//
// Durable state lives in a single JSON file under the instance root, rewritten
// atomically (journal.WriteFileAtomic) on every mutation — sized for V0's scale
// (concurrently-claimed backlog items, not a database's worth of rows). It is
// designed for one embedded scheduler per instance (SCH-040: no separate
// scheduler service), so an in-process mutex is the correct atomicity
// primitive — not cross-process file locking.
type ClaimLedger struct {
	mu      sync.Mutex
	path    string
	entries map[string]ClaimEntry
	now     func() time.Time
	log     *journal.InstanceLog // optional; nil-safe
}

// LedgerOption configures a ClaimLedger.
type LedgerOption func(*ClaimLedger)

// WithLedgerClock overrides the time source (for deterministic tests).
func WithLedgerClock(now func() time.Time) LedgerOption {
	return func(l *ClaimLedger) { l.now = now }
}

// WithInstanceLog journals claim.acquired/claim.released transitions to the
// instance journal (§4/§6). Optional — a ledger with no log still functions,
// it just isn't observable via `cat scheduler/events.jsonl`.
func WithInstanceLog(log *journal.InstanceLog) LedgerOption {
	return func(l *ClaimLedger) { l.log = log }
}

// OpenClaimLedger loads the ledger at path (a JSON file under the instance's
// scheduler dir), creating an empty one if absent.
func OpenClaimLedger(path string, opts ...LedgerOption) (*ClaimLedger, error) {
	l := &ClaimLedger{path: path, entries: map[string]ClaimEntry{}, now: time.Now}
	for _, opt := range opts {
		opt(l)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil // fresh ledger
		}
		return nil, fmt.Errorf("localscheduler: read claim ledger: %w", err)
	}
	if len(data) == 0 {
		return l, nil
	}
	if err := json.Unmarshal(data, &l.entries); err != nil {
		return nil, fmt.Errorf("localscheduler: parse claim ledger %q: %w", path, err)
	}
	return l, nil
}

// Claim attempts to atomically acquire itemID for runID under workflow, for
// leaseDuration. It fails (ok=false, holder=the current owner's run id) if a
// live (non-expired) lease is already held by a DIFFERENT run. An idempotent
// re-claim by the same runID succeeds and renews the lease — a retried
// backlog-query stage attempt (same run, same item) must not be refused by its
// own earlier claim.
//
// leaseDuration must be positive (issue #235, edge 1): a non-positive
// duration computes ExpiresAt <= ClaimedAt, so the entry is expired() at the
// moment it's written — expired() is exactly what the exclusivity guard
// below checks, so a non-positive lease would admit it unconditionally and
// let a second run silently co-own the same item. Fails closed before any
// ledger mutation, independent of ledger state, so this can never be
// bypassed by a caller-supplied duration (e.g. a workflow's leaseDuration
// input) reaching a live-lease branch that skips validation.
func (l *ClaimLedger) Claim(itemID, runID, workflow string, leaseDuration time.Duration) (ok bool, holder string, err error) {
	if leaseDuration <= 0 {
		return false, "", fmt.Errorf("localscheduler: lease duration must be positive, got %s", leaseDuration)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if existing, held := l.entries[itemID]; held && !existing.expired(now) && existing.RunID != runID {
		return false, existing.RunID, nil
	}

	prev, hadPrev := l.entries[itemID]
	entry := ClaimEntry{
		ItemID:    itemID,
		RunID:     runID,
		Workflow:  workflow,
		ClaimedAt: now,
		ExpiresAt: now.Add(leaseDuration),
	}
	l.entries[itemID] = entry
	if err := l.persist(); err != nil {
		// Roll back the in-memory mutation so a failed persist leaves the item
		// exactly as it was — claimable if it was unheld, or still held by its
		// prior owner on an idempotent renewal. The ledger's in-memory and durable
		// state must never diverge: without this, a persist blip would strand the
		// item as un-claimable in memory while nothing durably holds it.
		if hadPrev {
			l.entries[itemID] = prev
		} else {
			delete(l.entries, itemID)
		}
		return false, "", err
	}
	l.journal(journal.EventClaimAcquired, entry)
	return true, runID, nil
}

// Release explicitly releases a claim (run finished, failed, or crash-recovery
// determined it orphaned). Releasing a claim not held (already released, held
// by a different run, or never claimed) is a no-op, not an error — normal
// completion and crash-recovery can race to release the same item, and both
// outcomes are fine as long as exactly one claimant ever wins.
func (l *ClaimLedger) Release(itemID, runID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, held := l.entries[itemID]
	if !held || entry.RunID != runID {
		return nil
	}
	delete(l.entries, itemID)
	if err := l.persist(); err != nil {
		// Same rollback discipline as Claim: a failed persist must not leave
		// memory believing the item is free while the durable ledger (if the
		// write partially landed) or a crash-recovery reread still sees it
		// held — otherwise the item could be double-claimed while this run
		// still holds it, or the caller believes the release succeeded and
		// finalizes the run while the ledger still lists it as claimed.
		l.entries[itemID] = entry
		return err
	}
	l.journal(journal.EventClaimReleased, entry)
	return nil
}

// RecoverExpired releases every lease whose expiry has passed as of now and
// returns the released entries — the crash-recovery pass (SCH-021): a lease
// survives its owning run's crash only until it expires, at which point the
// item is claimable again exactly once. Call once at daemon startup (recovers
// leases orphaned by a prior crash) and periodically thereafter (catches a live
// run that overran its lease without crashing).
//
// Safety (WF-031): auto-releasing a lease whose owning run is still live but
// simply ran long invites double-processing — the freed item can be claimed
// by a second run while the first is still working it. This ledger's only
// heartbeat mechanism is Claim's own idempotent re-claim-by-same-runID path
// (see Claim's doc comment), which renews ExpiresAt; nothing currently drives
// it periodically. Until a caller wires that renewal, leaseDuration passed to
// Claim MUST be set well above the workflow's realistic max run duration, not
// tuned tightly — RecoverExpired trusts the lease at face value and has no
// way to distinguish "orphaned by a crash" from "still running, just slow."
//
// Issue #235 (edge 2): a ci-poll-bearing implementation run can exceed the
// OLD 2h DefaultClaimLease (cmd/goobers/backlogquery.go), which made this
// hazard reachable in the shipped config, not just theoretical. The chosen
// V0.2 mitigation is raising DefaultClaimLease comfortably above a realistic
// run's duration — not the liveness-aware renewal this comment already
// describes as the durable fix, which remains deferred to V1 hardening.
func (l *ClaimLedger) RecoverExpired(now time.Time) ([]ClaimEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var released []ClaimEntry
	for id, entry := range l.entries {
		if entry.expired(now) {
			delete(l.entries, id)
			released = append(released, entry)
		}
	}
	if len(released) == 0 {
		return nil, nil
	}
	if err := l.persist(); err != nil {
		// Roll back every deletion this pass made: a partial view (some
		// entries released in memory, none durably, none journaled) would
		// both strand those items as claimable-in-memory-only and discard,
		// via the (nil, err) return, the exact set the caller would need to
		// retry or reconcile — restoring them makes a failed pass a clean
		// no-op the caller can safely retry on its next periodic call.
		for _, entry := range released {
			l.entries[entry.ItemID] = entry
		}
		return nil, err
	}
	for _, entry := range released {
		l.journal(journal.EventClaimReleased, entry)
	}
	return released, nil
}

// Lookup returns the current entry for itemID, if any live or expired claim
// exists (for inspection/testing; does not distinguish expired from live —
// callers wanting only live claims should check ExpiresAt themselves or use
// RecoverExpired first).
func (l *ClaimLedger) Lookup(itemID string) (ClaimEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[itemID]
	return e, ok
}

// ForRun returns the entry runID currently holds, if any (for inspection;
// same expired/live caveat as Lookup). A workflow whose backlog-query stage
// claims at most one item per run (#131's implementation.yaml: maxItems=1)
// can use this to recover which item its own run is processing from a
// later stage — a downstream stage such as issue-close-out runs as its own
// OS process in its own worktree, several stages after backlog-query, with
// no other way to learn the claimed item's id (Task.InputsFrom only threads
// from the immediately preceding stage, not an arbitrary earlier one, and
// backlog-query's own worktree — where it wrote the claimed item as a result
// file — no longer exists by the time a later stage runs). If a run somehow
// holds more than one claim, the entry returned is unspecified — the ledger
// does not track per-run claim order.
func (l *ClaimLedger) ForRun(runID string) (ClaimEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.RunID == runID {
			return e, true
		}
	}
	return ClaimEntry{}, false
}

// persist rewrites the ledger file atomically. Caller holds l.mu.
func (l *ClaimLedger) persist() error {
	data, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("localscheduler: marshal claim ledger: %w", err)
	}
	if err := journal.WriteFileAtomic(l.path, data, 0o644); err != nil {
		return fmt.Errorf("localscheduler: persist claim ledger: %w", err)
	}
	return nil
}

// journal appends a claim transition to the instance log, if one is wired.
// Best-effort observability, not the durability mechanism (persist() above is)
// — a journal write failure here is deliberately swallowed rather than failing
// the claim/release operation the ledger already committed.
func (l *ClaimLedger) journal(eventType journal.EventType, entry ClaimEntry) {
	if l.log == nil {
		return
	}
	_ = l.log.Append(journal.Event{
		Type:     eventType,
		Name:     entry.ItemID,
		RunID:    entry.RunID,
		Workflow: entry.Workflow,
	})
}
