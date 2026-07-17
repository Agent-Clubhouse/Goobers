package localscheduler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// fakeSpanStarter records every scheduler span opened (issue #126).
type fakeSpanStarter struct {
	mu    sync.Mutex
	calls []telemetry.SchedulerAttributes
}

func (f *fakeSpanStarter) StartSchedulerSpan(ctx context.Context, attrs telemetry.SchedulerAttributes) (context.Context, telemetry.Span, error) {
	f.mu.Lock()
	f.calls = append(f.calls, attrs)
	f.mu.Unlock()
	return ctx, telemetry.Span{}, nil
}

func (f *fakeSpanStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeStarter records every Start call and returns a canned result. It blocks
// on a channel if one is set, so tests can control exactly when a run
// "finishes" and its condition slot is released.
type fakeStarter struct {
	mu     sync.Mutex
	starts []StartRequest
	block  chan struct{} // if non-nil, Start waits on it before returning
	result StartResult
	err    error
}

func (f *fakeStarter) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	f.mu.Lock()
	f.starts = append(f.starts, req)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	return f.result, f.err
}

func (f *fakeStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

func newTestScheduler(t *testing.T, entries []WorkflowEntry) (*Scheduler, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return New(entries, log), dir
}

func TestTickDispatchesDueWorkflow(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}})

	now := time.Now()
	sched.Tick(context.Background(), now.Add(2*time.Hour))

	waitForCount(t, func() int { return starter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawFired, sawStarted bool
	for _, ev := range events {
		if ev.Type == journal.EventTriggerFired && ev.Workflow == "implement" {
			sawFired = true
		}
		if ev.Type == journal.EventRunStarted && ev.Workflow == "implement" {
			sawStarted = true
		}
	}
	if !sawFired || !sawStarted {
		t.Fatalf("expected trigger.fired + run.started journaled: %+v", events)
	}
}

// waitForRunFinished polls the instance log at dir until a run.finished event
// for workflow appears, returning it — the dispatch goroutine journals this
// AFTER Start returns and after starter.count() is already visible to the
// caller, so a plain waitForCount on the start call races this event.
func waitForRunFinished(t *testing.T, dir, workflow string) journal.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := journal.ReadInstanceLog(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			if ev.Type == journal.EventRunFinished && ev.Workflow == workflow {
				return ev
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for run.finished for workflow %q", workflow)
	return journal.Event{}
}

// TestDispatchEchoesBusinessFailureCause is issue #710's scheduler-side
// acceptance: a business-failed StartResult (FailureStage/Code/Message
// populated, startErr nil — exactly what runner.Result/StartResult carry for
// a stage's own ResultFailure, per starter.go's field-for-field mirror)
// enriches the instance-journal run.finished echo with the actual cause,
// both as a human-readable status suffix and as structured Stage/Error
// fields — instead of the pre-fix bare status:"failed" that made #705's real
// cause invisible one level above the run's own journal.
func TestDispatchEchoesBusinessFailureCause(t *testing.T) {
	starter := &fakeStarter{result: StartResult{
		Phase: journal.PhaseFailed, FailureStage: "pr-select",
		FailureCode: "github_rate_limited", FailureMessage: "list pull requests: status 403, remaining 0",
	}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}})

	sched.Tick(context.Background(), time.Now().Add(2*time.Hour))

	ev := waitForRunFinished(t, dir, "implement")
	wantStatus := "failed (pr-select: github_rate_limited)"
	if ev.Status != wantStatus {
		t.Fatalf("run.finished status = %q, want %q", ev.Status, wantStatus)
	}
	if ev.Stage != "pr-select" {
		t.Fatalf("run.finished stage = %q, want pr-select", ev.Stage)
	}
	if ev.Error == nil || ev.Error.Code != "github_rate_limited" || ev.Error.Message != "list pull requests: status 403, remaining 0" {
		t.Fatalf("run.finished error = %+v, want code=github_rate_limited with the stage's own message", ev.Error)
	}
}

// TestDispatchEchoesInfraErrorUnchanged is issue #710's negative control (AC:
// "infra error (status:\"error: …\") unchanged"): a genuine Go dispatch error
// from Start (startErr != nil) must keep its exact pre-#710 echo shape — no
// Stage/Error enrichment, since FailureCode is meaningless on this path (the
// zero-value StartResult the fakeStarter returns alongside the error proves
// the echo doesn't accidentally read stale/zero failure fields either).
func TestDispatchEchoesInfraErrorUnchanged(t *testing.T) {
	starter := &fakeStarter{err: errors.New("dial tcp: connection refused")}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}})

	sched.Tick(context.Background(), time.Now().Add(2*time.Hour))

	ev := waitForRunFinished(t, dir, "implement")
	wantStatus := "error: dial tcp: connection refused"
	if ev.Status != wantStatus {
		t.Fatalf("run.finished status = %q, want %q (unchanged infra-error shape)", ev.Status, wantStatus)
	}
	if ev.Stage != "" || ev.Error != nil {
		t.Fatalf("run.finished stage=%q error=%+v, want both empty on the infra-error path", ev.Stage, ev.Error)
	}
}

func TestTickSkipsWhenConditionsExhausted(t *testing.T) {
	block := make(chan struct{})
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}})

	base := time.Now()
	// First tick admits and starts a run that blocks (holds its slot).
	sched.Tick(context.Background(), base.Add(time.Hour))
	waitForCount(t, func() int { return starter.count() }, 1)

	// Second due tick, one MaxConcurrentRuns=1 slot already held: must skip.
	sched.Tick(context.Background(), base.Add(2*time.Hour))
	close(block) // release the first run so the test can exit cleanly

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var skipped bool
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && ev.Reason == ReasonMaxParallel {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("expected a tick.skipped(max-parallel) event: %+v", events)
	}
	if starter.count() != 1 {
		t.Fatalf("starter should have been called exactly once, got %d", starter.count())
	}
}

// TestCronRunHoldsSlotThenManualTriggerRejected is issue #134's literal
// acceptance criterion: concurrent manual+cron admission respects
// maxConcurrentRuns. A cron-dispatched run holds the one available slot;
// a manual Trigger for the SAME workflow must be rejected by Conditions,
// not silently double-dispatch it.
func TestCronRunHoldsSlotThenManualTriggerRejected(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}})

	base := time.Now()
	sched.Tick(context.Background(), base.Add(time.Hour)) // cron fire holds the slot
	waitForCount(t, func() int { return starter.count() }, 1)

	_, err := sched.Trigger(context.Background(), "implement", base.Add(time.Minute))
	if err == nil {
		t.Fatal("expected the manual trigger to be rejected while the cron run holds the max-parallel slot")
	}
	if !strings.Contains(err.Error(), ReasonMaxParallel) {
		t.Fatalf("err = %v, want it to mention %q", err, ReasonMaxParallel)
	}
	if starter.count() != 1 {
		t.Fatalf("starter should have been called exactly once (cron only), got %d", starter.count())
	}
}

func TestManualTriggerBypassesCronButHonorsConditions(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules: nil, // manual-only: Tick alone would never fire this
		Starter:   starter,
	}})

	// A cron Tick does nothing for a manual-only workflow.
	sched.Tick(context.Background(), time.Now())
	if starter.count() != 0 {
		t.Fatalf("manual-only workflow should not fire from Tick: %d starts", starter.count())
	}

	runID, err := sched.Trigger(context.Background(), "curate", time.Now())
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if runID == "" {
		t.Fatal("expected Trigger to return the dispatched run's id")
	}
	waitForCount(t, func() int { return starter.count() }, 1)
	if got := starter.starts[0].Trigger.Kind; got != journal.TriggerManual {
		t.Fatalf("dispatched run's Trigger.Kind = %q, want %q (issue #134)", got, journal.TriggerManual)
	}
}

func TestTriggerUnknownWorkflowErrors(t *testing.T) {
	sched, _ := newTestScheduler(t, nil)
	if _, err := sched.Trigger(context.Background(), "nope", time.Now()); err == nil {
		t.Fatal("expected an error for an unknown workflow")
	}
}

// TestTriggerReasonIsManualNotScheduled is issue #134's fireReason fix: a
// manual Trigger must never journal trigger.fired with reason "scheduled" —
// the pre-fix bug that made a manual `goobers run` indistinguishable from a
// real cron fire in the instance journal.
func TestTriggerReasonIsManualNotScheduled(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}})

	if _, err := sched.Trigger(context.Background(), "curate", time.Now()); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	waitForCount(t, func() int { return starter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	for _, ev := range events {
		if ev.Type == journal.EventTriggerFired && ev.Workflow == "curate" {
			reason = ev.Reason
		}
	}
	if reason != "manual" {
		t.Fatalf("trigger.fired reason = %q, want \"manual\"", reason)
	}
}

// TestTriggerSkipRejectsWithReason is issue #134's other half: unlike a cron
// tick's silent skip, a human explicitly asked for this run, so a
// conditions-driven rejection must surface as an error, not a no-op.
func TestTriggerSkipRejectsWithReason(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}})

	if _, err := sched.Trigger(context.Background(), "curate", time.Now()); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitForCount(t, func() int { return starter.count() }, 1)

	_, err := sched.Trigger(context.Background(), "curate", time.Now())
	if err == nil {
		t.Fatal("expected the second concurrent Trigger to be rejected by run conditions")
	}
	if !strings.Contains(err.Error(), ReasonMaxParallel) {
		t.Fatalf("err = %v, want it to mention %q", err, ReasonMaxParallel)
	}
}

// TestSignalDispatchesOnlySubscribedWorkflows is #342's core acceptance: a
// signal fired by name dispatches every workflow subscribed to it and
// leaves unrelated workflows (subscribed to a different name, or not
// subscribed at all) untouched.
func TestSignalDispatchesOnlySubscribedWorkflows(t *testing.T) {
	subscribed := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	otherSignal := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	unsubscribed := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{
		{Workflow: "wants-deploy", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, Signals: []string{"deploy"}, Starter: subscribed},
		{Workflow: "wants-release", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, Signals: []string{"release"}, Starter: otherSignal},
		{Workflow: "manual-only", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, Starter: unsubscribed},
	})

	runIDs := sched.Signal(context.Background(), "deploy", time.Now())
	if len(runIDs) != 1 || runIDs[0] == "" {
		t.Fatalf("runIDs = %v, want exactly one non-empty run id", runIDs)
	}
	waitForCount(t, func() int { return subscribed.count() }, 1)

	if otherSignal.count() != 0 {
		t.Fatalf("workflow subscribed to a different signal should not have fired: %d starts", otherSignal.count())
	}
	if unsubscribed.count() != 0 {
		t.Fatalf("unsubscribed workflow should not have fired: %d starts", unsubscribed.count())
	}
}

// TestSignalUnknownNameReturnsEmptyNotError proves an unmatched signal name
// is a legitimate empty broadcast (zero subscribers), not an error — unlike
// Trigger's unknown-workflow case, which names one specific workflow a human
// explicitly asked for.
func TestSignalUnknownNameReturnsEmptyNotError(t *testing.T) {
	sched, _ := newTestScheduler(t, []WorkflowEntry{
		{Workflow: "wants-deploy", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, Signals: []string{"deploy"}, Starter: &fakeStarter{}},
	})
	runIDs := sched.Signal(context.Background(), "no-such-signal", time.Now())
	if len(runIDs) != 0 {
		t.Fatalf("runIDs = %v, want empty for an unmatched signal name", runIDs)
	}
}

// TestSignalRejectedSubscriberIsSkippedNotError is Signal's fan-out
// semantics: unlike Trigger (one named workflow, conditions-driven rejection
// is a caller-facing error), a signal with multiple subscribers must not let
// one rejected subscriber abort the broadcast — the admitted ones still
// fire, and Signal reports only the admitted run ids (no error return at
// all).
func TestSignalRejectedSubscriberIsSkippedNotError(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	blocked := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	free := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{
		{Workflow: "already-busy", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, Signals: []string{"deploy"}, Starter: blocked},
		{Workflow: "has-room", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, Signals: []string{"deploy"}, Starter: free},
	})

	// Occupy already-busy's one slot first via a direct Trigger.
	if _, err := sched.Trigger(context.Background(), "already-busy", time.Now()); err != nil {
		t.Fatalf("seed Trigger: %v", err)
	}
	waitForCount(t, func() int { return blocked.count() }, 1)

	runIDs := sched.Signal(context.Background(), "deploy", time.Now())
	if len(runIDs) != 1 {
		t.Fatalf("runIDs = %v, want exactly one admitted run (has-room only)", runIDs)
	}
	waitForCount(t, func() int { return free.count() }, 1)
	if blocked.count() != 1 {
		t.Fatalf("already-busy should still show only its one seeded start, got %d", blocked.count())
	}
}

// TestSignalReasonIsSignalNotScheduled mirrors #134's manual-Trigger fix for
// Signal: a signal-driven fire must journal trigger.fired with reason
// "signal", not fall through to "scheduled" (fireReason's default branch).
func TestSignalReasonIsSignalNotScheduled(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "wants-deploy",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Signals:   []string{"deploy"},
		Starter:   starter,
	}})

	runIDs := sched.Signal(context.Background(), "deploy", time.Now())
	if len(runIDs) != 1 {
		t.Fatalf("runIDs = %v, want exactly one", runIDs)
	}
	waitForCount(t, func() int { return starter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	var kind journal.TriggerKind
	for _, ev := range events {
		if ev.Type == journal.EventTriggerFired && ev.Workflow == "wants-deploy" {
			reason = ev.Reason
		}
		if ev.Type == journal.EventRunStarted && ev.Workflow == "wants-deploy" {
			kind = starter.starts[0].Trigger.Kind
		}
	}
	if reason != "signal" {
		t.Fatalf("trigger.fired reason = %q, want \"signal\"", reason)
	}
	if kind != journal.TriggerSignal {
		t.Fatalf("dispatched run's Trigger.Kind = %q, want %q", kind, journal.TriggerSignal)
	}
}

// TestDispatchRefusesWhenTriggerFiredJournalFails is issue #142/SCH-031: a
// failed trigger.fired append used to be silently swallowed, so dispatch
// would start a run whose firing was never durably recorded — on restart,
// ReconstructLastEval (which derives LastEval purely from trigger.fired
// history) would then replay that same nominal firing, double-dispatching a
// run for it. Refusing to dispatch when the append fails closes that gap.
func TestDispatchRefusesWhenTriggerFiredJournalFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Close the log before it's ever used: Append on a closed InstanceLog
	// deterministically returns journal.ErrClosed, forcing dispatch's
	// trigger.fired append to fail without relying on filesystem tricks.
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched := New([]WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}}, log)

	_, err = sched.Trigger(context.Background(), "curate", time.Now())
	if err == nil {
		t.Fatal("expected Trigger to fail when the trigger.fired journal append fails")
	}
	if !strings.Contains(err.Error(), "journal") {
		t.Fatalf("err = %v, want it to mention the journal failure", err)
	}
	if got := starter.count(); got != 0 {
		t.Fatalf("starter.count() = %d, want 0 — no run should start when trigger.fired didn't durably land", got)
	}
}

// TestReconcileRestoresBudgetWindowFromInstanceLog is issue #135's "budget
// amnesia" fix: Conditions' MaxRunsPerHour rolling window is in-memory only,
// so without reconstructing it from the instance journal's run.started
// history on restart, a crash-looping daemon would admit one extra
// catch-up-style fire per restart, silently exceeding the declared budget.
func TestReconcileRestoresBudgetWindowFromInstanceLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	now := time.Now()

	// Simulate a prior process instance admitting one run 10 minutes ago —
	// well within the 1-hour budget window Reconcile must restore.
	past, _, err := journal.OpenInstanceLog(dir, journal.WithClock(func() time.Time { return now.Add(-10 * time.Minute) }))
	if err != nil {
		t.Fatal(err)
	}
	if err := past.Append(journal.Event{Type: journal.EventRunStarted, Workflow: "curate", RunID: "prior-run"}); err != nil {
		t.Fatal(err)
	}
	if err := past.Close(); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched := New([]WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 1},
		Starter:   starter,
	}}, log)
	if err := sched.Reconcile(filepath.Join(t.TempDir(), "runs"), now); err != nil {
		t.Fatal(err)
	}

	// Without the fix, Conditions.starts starts empty every restart — this
	// Trigger would wrongly be admitted despite the budget already being spent.
	if _, err := sched.Trigger(context.Background(), "curate", now); err == nil {
		t.Fatal("expected the trigger to be rejected: budget already spent per the reconstructed instance-journal history")
	} else if !strings.Contains(err.Error(), ReasonBudget) {
		t.Fatalf("err = %v, want it to mention %q", err, ReasonBudget)
	}
}

// TestReconcileRateResetClearsBudgetWindow is #315's core: a rate-limit reset
// marker (written by `goobers reset-rate-limit`) raises the budget window's
// floor to the reset moment, so run.started history at or before it stops
// counting — letting an operator run again immediately without the old
// `rm -rf <instance>` workaround that destroyed runs/. This is the mirror of
// TestReconcileRestoresBudgetWindowFromInstanceLog: same spent-budget history,
// but with a reset written after it, so the trigger is now ADMITTED.
func TestReconcileRateResetClearsBudgetWindow(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	now := time.Now()

	// A prior run admitted 10 minutes ago — within the 1-hour window, so
	// without a reset the budget (MaxRunsPerHour: 1) is spent.
	past, _, err := journal.OpenInstanceLog(dir, journal.WithClock(func() time.Time { return now.Add(-10 * time.Minute) }))
	if err != nil {
		t.Fatal(err)
	}
	if err := past.Append(journal.Event{Type: journal.EventRunStarted, Workflow: "curate", RunID: "prior-run"}); err != nil {
		t.Fatal(err)
	}
	if err := past.Close(); err != nil {
		t.Fatal(err)
	}

	// Operator resets the rate window now — after that spent run.
	if err := WriteRateReset(dir, now.Add(-time.Minute)); err != nil {
		t.Fatalf("WriteRateReset: %v", err)
	}

	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched := New([]WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 1},
		Starter:   starter,
	}}, log)
	if err := sched.Reconcile(filepath.Join(t.TempDir(), "runs"), now); err != nil {
		t.Fatal(err)
	}

	// The reset floor is newer than the 10-min-ago run.started, so that history
	// no longer counts — the trigger is admitted despite the pre-reset budget
	// having been spent.
	if _, err := sched.Trigger(context.Background(), "curate", now); err != nil {
		t.Fatalf("expected the trigger to be admitted after a rate reset, got: %v", err)
	}
}

// TestReconcileStaleRateResetIsNoOp proves a reset older than the rolling
// window has no effect: the window has already advanced past it, so the normal
// 1-hour cutoff governs and a still-in-window spent run keeps the budget spent.
func TestReconcileStaleRateResetIsNoOp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	now := time.Now()

	past, _, err := journal.OpenInstanceLog(dir, journal.WithClock(func() time.Time { return now.Add(-10 * time.Minute) }))
	if err != nil {
		t.Fatal(err)
	}
	if err := past.Append(journal.Event{Type: journal.EventRunStarted, Workflow: "curate", RunID: "prior-run"}); err != nil {
		t.Fatal(err)
	}
	if err := past.Close(); err != nil {
		t.Fatal(err)
	}

	// A reset from 2 hours ago — older than the budget window, so it must not
	// resurrect budget for the still-in-window (10-min-ago) run.
	if err := WriteRateReset(dir, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched := New([]WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 1},
		Starter:   starter,
	}}, log)
	if err := sched.Reconcile(filepath.Join(t.TempDir(), "runs"), now); err != nil {
		t.Fatal(err)
	}

	if _, err := sched.Trigger(context.Background(), "curate", now); err == nil {
		t.Fatal("expected the trigger to be rejected: a stale reset must not resurrect a spent budget")
	} else if !strings.Contains(err.Error(), ReasonBudget) {
		t.Fatalf("err = %v, want it to mention %q", err, ReasonBudget)
	}
}

// TestTickSkipsOnBudgetExhaustion is the budget half of the run-conditions
// acceptance criterion (the max-parallel half is TestTickSkipsWhenConditions
// Exhausted): once MaxRunsPerHour is spent, further due ticks skip and journal
// ReasonBudget, never fail.
func TestTickSkipsOnBudgetExhaustion(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "curate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 1},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}})

	base := time.Now()
	sched.Tick(context.Background(), base.Add(time.Hour)) // uses the hourly budget
	waitForCount(t, func() int { return starter.count() }, 1)

	sched.Tick(context.Background(), base.Add(2*time.Hour)) // due again, budget spent

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawBudgetSkip bool
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && ev.Reason == ReasonBudget {
			sawBudgetSkip = true
		}
	}
	if !sawBudgetSkip {
		t.Fatalf("expected a tick.skipped(budget) event: %+v", events)
	}
	if starter.count() != 1 {
		t.Fatalf("starter should have been called exactly once (budget exhausted), got %d", starter.count())
	}
}

// TestMissedTickCatchUpJournaled is the missed-tick acceptance criterion at
// the Scheduler level: daemon downtime spanning several scheduled fires
// produces exactly one catch-up run, and the journaled trigger.fired event
// records it as a catch-up (not silently indistinguishable from an on-time fire).
func TestMissedTickCatchUpJournaled(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "nominate",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}})

	base := time.Now()
	// Simulate the daemon having been down for 5 scheduled fires.
	sched.Tick(context.Background(), base.Add(5*time.Hour))
	waitForCount(t, func() int { return starter.count() }, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var catchUpReason string
	for _, ev := range events {
		if ev.Type == journal.EventTriggerFired && ev.Workflow == "nominate" {
			catchUpReason = ev.Reason
		}
	}
	if catchUpReason == "" || catchUpReason == "scheduled" {
		t.Fatalf("expected the fire to be journaled as a catch-up, got reason %q", catchUpReason)
	}

	// The very next tick must not replay a backlog of the missed fires.
	sched.Tick(context.Background(), base.Add(5*time.Hour+time.Minute))
	if starter.count() != 1 {
		t.Fatalf("missed-tick collapse must not leave a backlog to replay: %d starts", starter.count())
	}
}

// TestRunDoesNotBusyPoll is the "no busy-polling: daemon idles between ticks"
// acceptance criterion: drive Run with a fake clock/timer and assert (a) it
// only re-evaluates when the injected timer channel fires — never more often
// — and (b) every requested wait duration is strictly positive, proving the
// loop blocks on a timer rather than spinning.
func TestRunDoesNotBusyPoll(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	base := time.Now()
	var mu sync.Mutex
	cur := base
	fc := newFakeClock(cur)

	sched := New([]WorkflowEntry{{
		Workflow:  "wf",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}}, log, WithClock(fc.Now, fc.After))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Advance through exactly 3 simulated ticks, each firing the workflow.
	for i := 0; i < 3; i++ {
		fc.awaitAfterCall(t)
		mu.Lock()
		cur = cur.Add(time.Hour)
		mu.Unlock()
		fc.advance(cur)
	}
	// Wait on the actual observable the assertion below checks — dispatch()
	// hands the Starter.Start call to its own goroutine (Run must never
	// block on a run), so the loop registering its next After call (what a
	// 4th awaitAfterCall would prove) does NOT happen-before that goroutine
	// incrementing starter's count. Waiting on the count itself closes that
	// race deterministically instead of relying on tick timing as a proxy.
	waitForCount(t, starter.count, 3)
	cancel()
	<-done

	if got := starter.count(); got < 3 {
		t.Fatalf("expected at least 3 dispatches from 3 controlled ticks, got %d", got)
	}
	for i, d := range fc.durations() {
		if d <= 0 {
			t.Fatalf("After call %d requested a non-positive duration %v — would busy-loop", i, d)
		}
	}
	// The number of After calls should track the number of controlled fires
	// (initial tick + one per advance), not run away on its own.
	if calls := len(fc.durations()); calls > 6 {
		t.Fatalf("too many After calls (%d) for 4 controlled advances — looks like busy-polling", calls)
	}
}

// TestDispatchEmitsSchedulerSpan is issue #126's local-scheduler acceptance:
// when WithTelemetry is configured, a dispatched tick opens exactly one
// scheduler decision span, attributed to the firing workflow. Before this
// fix, Scheduler had no telemetry seam at all — dispatch() never called
// StartSchedulerSpan, the direct parity gap vs internal/scheduler.Scheduler.
func TestDispatchEmitsSchedulerSpan(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	spans := &fakeSpanStarter{}
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	sched := New([]WorkflowEntry{{
		Workflow:  "implement",
		Gaggle:    "acme-web",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}}, log, WithTelemetry(spans))

	now := time.Now()
	sched.Tick(context.Background(), now.Add(2*time.Hour))

	waitForCount(t, starter.count, 1)
	waitForCount(t, spans.count, 1)

	spans.mu.Lock()
	got := spans.calls[0]
	spans.mu.Unlock()
	if got.Gaggle != "acme-web" || got.WorkflowID != "implement" || got.Action != "dispatch" {
		t.Fatalf("scheduler span attrs = %+v, want gaggle=acme-web workflowId=implement action=dispatch", got)
	}
}

// TestConcurrentTickDoesNotDoubleDispatch is issue #138's Tick race fix:
// Tick is exported specifically so a manual trigger and tests can call it
// outside the Run loop, which means overlapping calls are a real possibility,
// not just a hypothetical. Before the fix, Tick read a workflow's
// TriggerState, unlocked, evaluated it, then relocked to write back — two
// concurrent Tick calls could both read the same pre-fire state, both
// compute Fire=true, and both dispatch the same due firing. Racing many
// concurrent Tick calls at the same due instant must start exactly one run.
func TestConcurrentTickDoesNotDoubleDispatch(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1000},
		Schedules: []Schedule{fakeSchedule{d: time.Hour}},
		Starter:   starter,
	}})

	due := time.Now().Add(2 * time.Hour)
	const workers = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sched.Tick(context.Background(), due)
		}()
	}
	close(start)
	wg.Wait()

	waitForCount(t, func() int { return starter.count() }, 1)
	time.Sleep(20 * time.Millisecond) // let any erroneous second dispatch land
	if got := starter.count(); got != 1 {
		t.Fatalf("starter.count() = %d, want exactly 1 — concurrent Tick calls double-dispatched the same due firing", got)
	}
}

func waitForCount(t *testing.T, count func() int, want int) {
	t.Helper()
	// 10s (not the original 2s, issue #142's QA-gate stress flake): this is a
	// safety net against a genuine hang, not an expected duration — on a
	// machine running many concurrent agents/test suites, legitimate work
	// occasionally exceeds 2s under contention with no actual bug involved.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for count >= %d, got %d", want, count())
}

// fakeClock is a controllable Clock for no-busy-poll tests: Now() reads an
// atomically-updated instant, After() records the requested duration and
// returns a channel the test fires manually via advance().
type fakeClock struct {
	now atomic.Pointer[time.Time]

	mu       sync.Mutex
	ch       chan time.Time
	waiting  chan struct{}
	requests []time.Duration
}

func newFakeClock(start time.Time) *fakeClock {
	f := &fakeClock{ch: make(chan time.Time), waiting: make(chan struct{}, 8)}
	f.now.Store(&start)
	return f
}

func (f *fakeClock) Now() time.Time { return *f.now.Load() }

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	f.requests = append(f.requests, d)
	f.mu.Unlock()
	select {
	case f.waiting <- struct{}{}:
	default:
	}
	return f.ch
}

// awaitAfterCall blocks until Run has called After at least once since the
// last advance (i.e. it's idling on the timer, ready for the next controlled fire).
func (f *fakeClock) awaitAfterCall(t *testing.T) {
	t.Helper()
	select {
	case <-f.waiting:
	case <-time.After(10 * time.Second): // same contention margin as waitForCount, issue #142
		t.Fatal("timed out waiting for the scheduler loop to call After (idle-between-ticks)")
	}
}

// advance sets Now to t and fires the pending After channel once.
func (f *fakeClock) advance(t time.Time) {
	f.now.Store(&t)
	f.ch <- t
}

func (f *fakeClock) durations() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.requests...)
}
