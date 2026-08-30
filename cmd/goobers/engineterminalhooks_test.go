package main

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// hookRecorder captures the terminal-hook frame's calls in order, so the
// ORDER itself can be asserted rather than only the fact that each fired.
type hookRecorder struct {
	order []string

	existingFix []runner.ExistingFixOutcome
	blocked     []runner.BlockedOutcome
	failed      []runner.FailedOutcome
	prepared    []journal.RunPhase
	notified    []journal.RunPhase
	finalized   []journal.RunPhase

	failEvery error
}

func (r *hookRecorder) hooks(log *journal.InstanceLog) *engineTerminalHooks {
	return &engineTerminalHooks{
		log:     log,
		repoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		existingFix: func(_ context.Context, o runner.ExistingFixOutcome) error {
			r.order = append(r.order, "existingFix")
			r.existingFix = append(r.existingFix, o)
			return r.failEvery
		},
		blocked: func(_ context.Context, o runner.BlockedOutcome) error {
			r.order = append(r.order, "blocked")
			r.blocked = append(r.blocked, o)
			return r.failEvery
		},
		failed: func(_ context.Context, o runner.FailedOutcome) error {
			r.order = append(r.order, "failed")
			r.failed = append(r.failed, o)
			return r.failEvery
		},
		prepare: func(_ string, phase journal.RunPhase, _ terminalAnnotator) error {
			r.order = append(r.order, "prepare")
			r.prepared = append(r.prepared, phase)
			return r.failEvery
		},
		notify: func(_ string, phase journal.RunPhase, _ string) error {
			r.order = append(r.order, "notify")
			r.notified = append(r.notified, phase)
			return r.failEvery
		},
		finalize: func(_ string, phase journal.RunPhase) error {
			r.order = append(r.order, "finalize")
			r.finalized = append(r.finalized, phase)
			return r.failEvery
		},
	}
}

func hookTestLog(t *testing.T) (*journal.InstanceLog, string) {
	t.Helper()
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log, dir
}

// TestEngineTerminalHooksFireInTheRunnersOrder is the hazard the frame exists
// to close, asserted on the ORDER rather than on membership.
//
// The order is not cosmetic. FinalizeTerminal releases the claim ledger lease
// and the provider claim marker; every hook above it resolves the run's
// driving item FROM those claims for a run that claimed its item mid-walk. A
// frame that finalized first would leave every other hook with no item to act
// on, and the failure would be silent — the run's own journal still says
// `escalated`.
func TestEngineTerminalHooksFireInTheRunnersOrder(t *testing.T) {
	log, _ := hookTestLog(t)
	rec := &hookRecorder{}
	hooks := rec.hooks(log)

	hooks.run(context.Background(), engineTerminalOutcome{
		RunID:    "run-1",
		Gaggle:   "web",
		Workflow: "implementation",
		Phase:    journal.PhaseEscalated,
		Item:     &apiv1.BacklogItem{ID: "42"},
		Result: engine.RunResult{
			Status:     engine.StatusEscalated,
			FinalState: "implement",
			Outputs: map[string]apiv1.ResultEnvelope{
				"implement": {Status: apiv1.ResultBlocked, Summary: "waiting on 41"},
			},
		},
	})

	want := []string{"blocked", "prepare", "notify", "finalize"}
	if len(rec.order) != len(want) {
		t.Fatalf("hook order = %v, want %v", rec.order, want)
	}
	for i := range want {
		if rec.order[i] != want[i] {
			t.Fatalf("hook order = %v, want %v", rec.order, want)
		}
	}
}

// TestEngineTerminalHooksKeyOnJournalPhaseNotStatusWord is the E2 correction
// (#3874) made executable.
//
// engine.StatusBlocked — what an @abort routing target produces — projects to
// journal.PhaseABORTED, and engine.StatusEscalated projects to PhaseEscalated.
// That is NOT the mapping a reader guesses from the names, and a daemon that
// re-derived the phase from the status word would park an aborted run's item
// on goobers:needs-human and hand the branch cleanup the wrong phase.
func TestEngineTerminalHooksKeyOnJournalPhaseNotStatusWord(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   journal.RunPhase
	}{
		{engine.StatusCompleted, journal.PhaseCompleted},
		{engine.StatusBlocked, journal.PhaseAborted},
		{engine.StatusEscalated, journal.PhaseEscalated},
		{engine.StatusFailed, journal.PhaseFailed},
	} {
		if got := engineTerminalPhaseFor(engine.RunResult{Status: tc.status}, nil); got != tc.want {
			t.Errorf("engineTerminalPhaseFor(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
	// A walk-level error returns (RunResult{}, err): no status at all. It is
	// still a terminal the frame must fire for — claims do not release
	// themselves.
	if got := engineTerminalPhaseFor(engine.RunResult{}, errors.New("worker died")); got != journal.PhaseFailed {
		t.Errorf("a statusless engine run maps to %q, want %q", got, journal.PhaseFailed)
	}
}

// TestEngineTerminalHooksDistinguishBlockedFromEscalate: a stage reporting
// apiv1.ResultBlocked and an @escalate routing target BOTH end the walk at
// engine.StatusEscalated and both project to PhaseEscalated. Only the first
// is a blocked outcome with a reason and blockers to record. The two are told
// apart exactly as internal/runner tells them apart — by the FINAL STAGE'S
// own reported status — and a frame that keyed on the phase alone would file
// a spurious blocked record (parking the item on goobers:needs-human) for
// every escalation.
func TestEngineTerminalHooksDistinguishBlockedFromEscalate(t *testing.T) {
	log, _ := hookTestLog(t)
	rec := &hookRecorder{}
	rec.hooks(log).run(context.Background(), engineTerminalOutcome{
		RunID:    "run-esc",
		Gaggle:   "web",
		Workflow: "implementation",
		Phase:    journal.PhaseEscalated,
		Item:     &apiv1.BacklogItem{ID: "42"},
		Result: engine.RunResult{
			Status:     engine.StatusEscalated,
			FinalState: "review",
			// A routed @escalate: the final stage SUCCEEDED, the route sent
			// the run to the escalation terminal.
			Outputs: map[string]apiv1.ResultEnvelope{
				"review": {Status: apiv1.ResultSuccess, Summary: "needs a human eye"},
			},
		},
	})
	if len(rec.blocked) != 0 {
		t.Fatalf("a routed @escalate filed %d blocked records; the item would be parked on goobers:needs-human for a run that was never blocked", len(rec.blocked))
	}
	for _, name := range rec.order {
		if name == "blocked" {
			t.Fatal("blocked handler fired for a routed escalation")
		}
	}
}

// TestEngineTerminalHooksRecordBlockedReasonAndBlockers proves the blocked
// record is built from the ENVELOPE's own fields, through the same exported
// runner helpers the local path uses. A second parse in the daemon would let
// the same stage produce a different blocked record depending on which driver
// walked it.
func TestEngineTerminalHooksRecordBlockedReasonAndBlockers(t *testing.T) {
	log, _ := hookTestLog(t)
	rec := &hookRecorder{}
	rec.hooks(log).run(context.Background(), engineTerminalOutcome{
		RunID:    "run-blocked",
		Gaggle:   "web",
		Workflow: "implementation",
		Phase:    journal.PhaseEscalated,
		Item:     &apiv1.BacklogItem{ID: "42"},
		Result: engine.RunResult{
			Status:     engine.StatusEscalated,
			FinalState: "implement",
			Outputs: map[string]apiv1.ResultEnvelope{
				"implement": {
					Status:  apiv1.ResultBlocked,
					Summary: "needs the schema migration first",
					Outputs: map[string]any{"blockedBy": "41,42"},
				},
			},
		},
	})
	if len(rec.blocked) != 1 {
		t.Fatalf("blocked handler fired %d times, want 1", len(rec.blocked))
	}
	got := rec.blocked[0]
	if got.Stage != "implement" {
		t.Errorf("blocked stage = %q, want implement", got.Stage)
	}
	if got.Reason != "needs the schema migration first" {
		t.Errorf("blocked reason = %q, want the stage's stated reason", got.Reason)
	}
	if got.ItemID != "42" {
		t.Errorf("blocked itemID = %q, want 42", got.ItemID)
	}
	// #2961: an item cannot block itself, and the driving item is 42.
	for _, blocker := range got.Blockers {
		if blocker == "42" {
			t.Errorf("blockers %v retain the driving item; an item cannot block itself (#2961)", got.Blockers)
		}
	}
	if len(got.Blockers) != 1 || got.Blockers[0] != "41" {
		t.Errorf("blockers = %v, want [41]", got.Blockers)
	}
}

// TestEngineTerminalHooksExistingFixArm mirrors internal/runner's #3236 arm:
// an implement stage that completed with no-work because the fix already
// landed on main. Without it the item keeps its in-progress labels and is
// never reclaimed.
func TestEngineTerminalHooksExistingFixArm(t *testing.T) {
	log, _ := hookTestLog(t)
	rec := &hookRecorder{}
	rec.hooks(log).run(context.Background(), engineTerminalOutcome{
		RunID: "run-fix", Gaggle: "web", Workflow: "implementation",
		Phase: journal.PhaseCompleted,
		Item:  &apiv1.BacklogItem{ID: "42"},
		Result: engine.RunResult{
			Status:     engine.StatusCompleted,
			FinalState: "implement",
			NoWork:     true,
			Outputs: map[string]apiv1.ResultEnvelope{
				"implement": {Status: apiv1.ResultNoWork, Outputs: map[string]any{"existingFixCommit": "abc123"}},
			},
		},
	})
	if len(rec.existingFix) != 1 {
		t.Fatalf("existing-fix handler fired %d times, want 1", len(rec.existingFix))
	}
	if rec.existingFix[0].Commit != "abc123" || rec.existingFix[0].ItemID != "42" {
		t.Errorf("existing-fix outcome = %+v, want commit abc123 for item 42", rec.existingFix[0])
	}
	if rec.order[0] != "existingFix" {
		t.Errorf("hook order = %v, want existingFix first: it strips the labels that would otherwise let the item be reclaimed before the claim release", rec.order)
	}
}

// TestEngineTerminalHooksSkipExistingFixWithoutACommit: no-work with no
// declared commit is an ordinary no-work run (an empty backlog query, a stage
// that found nothing), not "the fix already exists". Firing the #3236 arm for
// it would strip an item's labels on the strength of no evidence at all.
func TestEngineTerminalHooksSkipExistingFixWithoutACommit(t *testing.T) {
	log, _ := hookTestLog(t)
	rec := &hookRecorder{}
	rec.hooks(log).run(context.Background(), engineTerminalOutcome{
		RunID: "run-nowork", Gaggle: "web", Workflow: "implementation",
		Phase: journal.PhaseCompleted,
		Result: engine.RunResult{
			Status: engine.StatusCompleted, FinalState: "implement", NoWork: true,
			Outputs: map[string]apiv1.ResultEnvelope{"implement": {Status: apiv1.ResultNoWork}},
		},
	})
	if len(rec.existingFix) != 0 {
		t.Fatalf("existing-fix fired for a no-work run with no declared commit: %+v", rec.existingFix)
	}
}

// TestEngineTerminalHooksAlwaysFinalizeEvenWhenAHandlerFails is the leak
// guard. Every handler above finalize is best-effort policy; finalize is the
// one that releases the claim ledger lease and retires the provider claim
// marker. A frame that aborted on the first handler error would hold the run's
// claims until the lease expired — the exact leak the frame exists to prevent,
// reached from the error path.
func TestEngineTerminalHooksAlwaysFinalizeEvenWhenAHandlerFails(t *testing.T) {
	log, dir := hookTestLog(t)
	rec := &hookRecorder{failEvery: errors.New("provider is down")}
	rec.hooks(log).run(context.Background(), engineTerminalOutcome{
		RunID: "run-leak", Gaggle: "web", Workflow: "implementation",
		Phase: journal.PhaseFailed,
		Result: engine.RunResult{
			Status: engine.StatusFailed, FinalState: "implement",
			FailureCode: "stage_failed", FailureMessage: "boom",
		},
	})
	if len(rec.finalized) != 1 {
		t.Fatalf("finalize fired %d times when every handler failed, want 1; the run's claims would leak until the lease expired", len(rec.finalized))
	}
	if rec.finalized[0] != journal.PhaseFailed {
		t.Errorf("finalize got phase %q, want %q", rec.finalized[0], journal.PhaseFailed)
	}
	// The failures must still be VISIBLE, in the instance log rather than the
	// run's own (closed, normatively complete) journal.
	events := instanceLogEventsForRun(t, dir, "run-leak")
	var errorEvents int
	for _, ev := range events {
		if ev.Type == journal.EventError {
			errorEvents++
		}
	}
	if errorEvents == 0 {
		t.Fatalf("no terminal-hook failures recorded in the instance log; %d events for the run", len(events))
	}
}

// TestEngineTerminalHooksFailedArmSuppliesAWalkLevelCode: an engine run that
// died at the WALK level returns (RunResult{}, err) — no status, no final
// state, no failure code. The #1054 failure trace must still carry a code, or
// the failure-streak circuit breaker and the read model both see an
// unclassified terminal.
func TestEngineTerminalHooksFailedArmSuppliesAWalkLevelCode(t *testing.T) {
	log, _ := hookTestLog(t)
	rec := &hookRecorder{}
	rec.hooks(log).run(context.Background(), engineTerminalOutcome{
		RunID: "run-walk-fail", Gaggle: "web", Workflow: "implementation",
		Phase: journal.PhaseFailed,
		Err:   errors.New("worker lost"),
	})
	if len(rec.failed) != 1 {
		t.Fatalf("failed handler fired %d times, want 1", len(rec.failed))
	}
	if rec.failed[0].Code != engineWalkFailureCode {
		t.Errorf("failure code = %q, want %q", rec.failed[0].Code, engineWalkFailureCode)
	}
	if rec.failed[0].Cause != "worker lost" {
		t.Errorf("failure cause = %q, want the workflow's own error", rec.failed[0].Cause)
	}
}

// TestEngineStartResultPreservesNoWork is the silent-loss hazard.
//
// NoWork is `omitempty` on the wire and drives the scheduler's idle backoff
// (recordScheduledPollResult). A run that found nothing to do looks exactly
// like a successful one without it, so an engine lane that dropped it would
// re-tick a genuinely empty backlog at full schedule rate forever, burning
// provider quota — the reason E2 (#3874) plumbed NoWork through RunResult at
// all.
func TestEngineStartResultPreservesNoWork(t *testing.T) {
	res := engine.RunResult{Status: engine.StatusCompleted, FinalState: "implement", NoWork: true}
	got := engineStartResult(res, journal.PhaseCompleted, nil)
	if !got.NoWork {
		t.Fatal("engineStartResult dropped NoWork; the scheduler would re-tick an empty backlog at full rate forever")
	}
	if got.Phase != journal.PhaseCompleted || got.FinalState != "implement" {
		t.Errorf("engineStartResult = %+v, want the completed phase and final state carried through", got)
	}
	// A non-failed terminal must not carry failure fields: the scheduler
	// records them verbatim, and a completed run with a FailureStage set is
	// a run the read model reports as broken.
	if got.FailureStage != "" || got.FailureCode != "" || got.FailureMessage != "" {
		t.Errorf("engineStartResult = %+v, want no failure fields on a completed run", got)
	}
}

// TestEngineStartResultCarriesFailureDetail is the other side: a failed run
// must reach the scheduler with a code, falling back to the walk-level one,
// and with the workflow's own error as the message when no stage claimed it.
func TestEngineStartResultCarriesFailureDetail(t *testing.T) {
	got := engineStartResult(engine.RunResult{}, journal.PhaseFailed, errors.New("worker lost"))
	if got.FailureCode != engineWalkFailureCode {
		t.Errorf("FailureCode = %q, want %q", got.FailureCode, engineWalkFailureCode)
	}
	if got.FailureMessage != "worker lost" {
		t.Errorf("FailureMessage = %q, want the workflow's error", got.FailureMessage)
	}

	stageFailed := engine.RunResult{
		Status: engine.StatusFailed, FinalState: "implement",
		FailureCode: "stage_failed", FailureMessage: "exit 1",
	}
	got = engineStartResult(stageFailed, journal.PhaseFailed, nil)
	if got.FailureStage != "implement" || got.FailureCode != "stage_failed" || got.FailureMessage != "exit 1" {
		t.Errorf("engineStartResult = %+v, want the stage's own failure detail preserved", got)
	}
}
