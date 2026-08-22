package localscheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// RunLivenessProbe answers whether the run holding a claim is still executing
// (DS6, docs/design/distributed-state-and-coordination.md §6/§10). The claim
// LEDGER decides which claims are candidates for renewal; a probe decides
// whether each holding run is live. Liveness has two sources, composed by the
// caller: the daemon's own in-process tracking (today's #2014 signal, still
// valid) and — when `engine:` is configured — an open engine workflow, which
// survives a daemon restart that loses the in-process tracking.
//
// Answer semantics: (true, nil) is live; (false, nil) is DEFINITIVELY not
// live (the run is closed or vanished); a non-nil error means liveness is
// UNKNOWN. Renewal treats unknown as live (fail-live): per DS6 only a closed
// or vanished workflow may let a lease lapse — an unreachable engine proves
// neither, and the cost of renewing a dead run's claim for one more lease is
// bounded, while reaping a live run's claim hands its item to a second run.
type RunLivenessProbe interface {
	RunLive(ctx context.Context, runID string) (bool, error)
}

// TrackedRunLiveness adapts an in-process tracked-run enumeration (the
// daemon's registry of runs it is actively driving) into a RunLivenessProbe.
// tracked is re-evaluated on every probe call, so a probe built at daemon
// startup — before crash-resume has tracked anything — sees later-tracked
// runs without being rebuilt.
func TrackedRunLiveness(tracked func() []string) RunLivenessProbe {
	return trackedRunLiveness{tracked: tracked}
}

type trackedRunLiveness struct{ tracked func() []string }

func (p trackedRunLiveness) RunLive(_ context.Context, runID string) (bool, error) {
	for _, id := range p.tracked() {
		if id == runID {
			return true, nil
		}
	}
	return false, nil
}

// CompositeRunLiveness reports a run live when ANY probe does. Probes are
// consulted in order and short-circuit on the first live answer. A probe
// error does not mask a later probe's live answer; when no probe answers
// live, any errors are joined and returned so the caller can fail live.
func CompositeRunLiveness(probes ...RunLivenessProbe) RunLivenessProbe {
	return compositeRunLiveness{probes: probes}
}

type compositeRunLiveness struct{ probes []RunLivenessProbe }

func (p compositeRunLiveness) RunLive(ctx context.Context, runID string) (bool, error) {
	var errs []error
	for _, probe := range p.probes {
		live, err := probe.RunLive(ctx, runID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if live {
			return true, nil
		}
	}
	return false, errors.Join(errs...)
}

// ProbeLiveClaimHolders probes each distinct RunID holding an entry and
// returns the set renewal must treat as live. A probe ERROR fails live — the
// run is included in the returned set — with the error joined into the second
// return for reporting: renewal proceeding on an unknown answer is the safe
// direction (see RunLivenessProbe), and the caller decides how loudly to
// surface the degraded probe. Run IDs are probed in sorted order so probe
// traffic and error text are deterministic.
func ProbeLiveClaimHolders(ctx context.Context, entries []ClaimEntry, probe RunLivenessProbe) (map[string]bool, error) {
	runIDs := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if _, dup := seen[entry.RunID]; dup {
			continue
		}
		seen[entry.RunID] = struct{}{}
		runIDs = append(runIDs, entry.RunID)
	}
	sort.Strings(runIDs)

	live := make(map[string]bool, len(runIDs))
	var errs []error
	for _, runID := range runIDs {
		ok, err := probe.RunLive(ctx, runID)
		if err != nil {
			live[runID] = true
			errs = append(errs, fmt.Errorf("probe liveness of run %s: %w (renewing fail-live)", runID, err))
			continue
		}
		if ok {
			live[runID] = true
		}
	}
	return live, errors.Join(errs...)
}

// RenewRuns extends, from now, the lease on every entry currently held by a
// run in live, and returns the renewed entries. Only entries present in THIS
// ledger are renewed — a caller that probed liveness against an earlier
// snapshot (outside the claims lock, since an engine probe can block on
// network) cannot resurrect a claim released between snapshot and renewal,
// because a released claim has no current entry to renew.
func (l *ClaimLedger) RenewRuns(live map[string]bool, leaseDuration time.Duration) ([]ClaimEntry, error) {
	var renewed []ClaimEntry
	for _, entry := range l.Snapshot() {
		if !live[entry.RunID] {
			continue
		}
		ok, err := l.RenewEntry(entry, leaseDuration)
		if err != nil {
			return renewed, fmt.Errorf("renew claim %s for run %s: %w", entry.ItemID, entry.RunID, err)
		}
		if ok {
			renewed = append(renewed, entry)
		}
	}
	return renewed, nil
}

// RecoveryGate enforces DS6's load-bearing startup ordering
// (docs/design/distributed-state-and-coordination.md §10): on daemon start
// the renewal set must be rebuilt from the ledger plus run liveness BEFORE
// RecoverExpired is permitted to run. A restarted daemon's in-process run
// tracking is empty while a distributed run keeps executing on the engine;
// reaping in that gap would hand a live run's item to a second run. The gate
// starts closed and opens exactly once, when a renewal rebuild has durably
// completed (MarkRenewalRebuilt); until then every reap pass that consults it
// must be a no-op. A rebuild whose LEDGER write failed must not open the gate
// — a probe-only degradation may (those runs were renewed fail-live).
//
// A nil *RecoveryGate permits recovery: callers with no startup renewal duty
// (one-shot `goobers run`/`goobers signal` paths) keep their existing
// recover-at-setup behavior unchanged.
type RecoveryGate struct {
	mu             sync.Mutex
	renewalRebuilt bool
}

// NewRecoveryGate returns a closed gate.
func NewRecoveryGate() *RecoveryGate { return &RecoveryGate{} }

// MarkRenewalRebuilt records that a ledger-driven renewal pass completed,
// permitting recovery from now on. Idempotent.
func (g *RecoveryGate) MarkRenewalRebuilt() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.renewalRebuilt = true
}

// RecoveryPermitted reports whether RecoverExpired-style reaping may run.
// Nil-safe: a nil gate always permits.
func (g *RecoveryGate) RecoveryPermitted() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.renewalRebuilt
}
