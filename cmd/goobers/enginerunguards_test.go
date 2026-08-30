package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// enginerunguards_test.go proves the daemon's decisions about a run it does
// not drive, at the seams those decisions are actually made:
//
//   - the startup resume scan (cmd/goobers/daemon.go),
//   - the stall sweep (cmd/goobers/stalledruns.go),
//   - `run abort` / `run cancel` (cmd/goobers/run.go),
//   - the HITL intervention service (cmd/goobers/interventions.go).
//
// Each drives the real function over a real run directory. The Temporal side
// is a fake engineWorkflowClient — the same shape internal/engine's own
// liveness tests use — so the assertions are about the daemon's behaviour,
// not about a Temporal server.

// fakeEngineWorkflows records what the daemon asked the engine to do.
type fakeEngineWorkflows struct {
	mu        sync.Mutex
	described []string
	awaited   []string
	cancelled []string

	notFound    bool
	describeErr error
	cancelErr   error
	status      enumspb.WorkflowExecutionStatus
	getErr      error
	// gate, when non-nil, blocks Get until closed — standing in for a
	// workflow that is still executing while the daemon holds the attachment.
	gate chan struct{}
	// result is what the workflow RETURNED, decoded into the caller's out
	// parameter. The resume scan discards it; the engine starter (#3876)
	// needs it, because the phase it reports to the scheduler and the
	// terminal hooks it fires are both derived from it.
	result engine.RunResult
	// workflowIDs, when non-empty, is the run-id -> workflow-id mapping a
	// scheduled engine run needs: describing the RUN id yields NotFound for
	// one, which is why the guards carry a resolver at all.
	workflowIDs map[string]string
}

func (f *fakeEngineWorkflows) DescribeWorkflowExecution(_ context.Context, workflowID, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.mu.Lock()
	f.described = append(f.described, workflowID)
	f.mu.Unlock()
	if f.notFound || (len(f.workflowIDs) > 0 && !f.knownWorkflowID(workflowID)) {
		return nil, serviceerror.NewNotFound("workflow not found")
	}
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	status := f.status
	if status == enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED {
		status = enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: status},
	}, nil
}

func (f *fakeEngineWorkflows) GetWorkflow(_ context.Context, workflowID, _ string) client.WorkflowRun {
	return &fakeWorkflowRun{parent: f, id: workflowID}
}

func (f *fakeEngineWorkflows) CancelWorkflow(_ context.Context, workflowID, _ string) error {
	f.mu.Lock()
	f.cancelled = append(f.cancelled, workflowID)
	f.mu.Unlock()
	return f.cancelErr
}

// knownWorkflowID reports whether workflowID is one the fake actually hosts.
func (f *fakeEngineWorkflows) knownWorkflowID(workflowID string) bool {
	for _, id := range f.workflowIDs {
		if id == workflowID {
			return true
		}
	}
	return false
}

func (f *fakeEngineWorkflows) snapshot() (described, awaited, cancelled []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.described...), append([]string(nil), f.awaited...), append([]string(nil), f.cancelled...)
}

type fakeWorkflowRun struct {
	parent *fakeEngineWorkflows
	id     string
}

func (r *fakeWorkflowRun) GetID() string    { return r.id }
func (r *fakeWorkflowRun) GetRunID() string { return "" }

func (r *fakeWorkflowRun) Get(ctx context.Context, out any) error {
	r.parent.mu.Lock()
	r.parent.awaited = append(r.parent.awaited, r.id)
	gate := r.parent.gate
	getErr := r.parent.getErr
	result := r.parent.result
	r.parent.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if getErr != nil {
		return getErr
	}
	if decoded, ok := out.(*engine.RunResult); ok && decoded != nil {
		*decoded = result
	}
	return nil
}

func (r *fakeWorkflowRun) GetWithOptions(ctx context.Context, valuePtr any, _ client.WorkflowRunGetOptions) error {
	return r.Get(ctx, valuePtr)
}

// createDriverRun writes a non-terminal run journal with an explicit driver —
// the on-disk shape the daemon scans. driver "" is a runner-driven run (every
// run.yaml written before this field existed).
func createDriverRun(t *testing.T, runsDir, runID, workflowName, gaggle string, driver journal.RunDriver, at time.Time, controls *apiv1.RunControls) {
	t.Helper()
	clock := at
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: workflowName, WorkflowVersion: 1, Gaggle: gaggle,
		Driver:      driver,
		RunControls: controls,
		Trigger:     journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("local-ci")
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "local-ci", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func runEventCount(t *testing.T, runsDir, runID string) int {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}

// TestResumeScanReattachesEngineDrivenRunInsteadOfResumingIt is the verified
// hazard at Goobers-e2e-core@076b4de9, driven through the exact function the
// daemon calls at startup.
//
// An engine-driven run's journal passes every check resumeInterruptedRuns
// makes — it is journal.PhaseRunning, its workflow resolves in config, its
// WF-016 pins verify — so before this change the restarted daemon called
// Runner.Resume on it and walked the run a second time in-process while
// goobers-worker kept walking it on Temporal. The assertions are the two
// halves of "the daemon stopped driving it": no `action: resumed` recovery
// annotation and an untouched run journal, plus a describe/await against the
// engine proving the daemon did attach rather than merely ignore the run.
func TestResumeScanReattachesEngineDrivenRunInsteadOfResumingIt(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "engine-driven-interrupted"
	createDriverRun(t, l.RunsDir(), runID, "default-implement", "example", journal.DriverEngine, time.Now(), nil)
	eventsBefore := runEventCount(t, l.RunsDir(), runID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = setup.Shutdown(context.Background()) }()
	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// The workflow is still executing: Get blocks until the test releases it,
	// which is exactly the state a mid-run goobers-api restart finds.
	gate := make(chan struct{})
	defer close(gate)
	fake := &fakeEngineWorkflows{gate: gate}
	guards := &engineRunGuards{client: fake}

	var released []string
	var releasedMu sync.Mutex
	resumed, warned, reattached, err := resumeInterruptedRunsWithRunners(
		ctx, l, setup.Runners, setup.LegacyRunner, setup.RunnerRegistry, guards,
		setup.Machines, setup.GooberDigests, setup.RepoRefs, setup.InstanceLog,
		setup.Telemetry, setup.RollupDB, setup.Watermarks,
		func(runID, workflowName string) {
			releasedMu.Lock()
			released = append(released, runID)
			releasedMu.Unlock()
			sched.ReleaseReconciled(runID, workflowName)
		},
		&wg,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 0 {
		t.Fatalf("resumed = %v, want none — an engine-driven run must never be walked by this process", resumed)
	}
	if len(warned) != 0 {
		t.Fatalf("warned = %v, want none — the run's workflow resolves fine", warned)
	}
	if len(reattached) != 1 || reattached[0] != runID {
		t.Fatalf("reattached = %v, want [%s]", reattached, runID)
	}

	// The drain WaitGroup must not have grown: SIGTERM cannot be made to wait
	// for a run this process is not executing.
	wg.Wait()

	// The run's own journal is byte-for-byte where the engine left it. A
	// second driver would have appended stage events here.
	if got := runEventCount(t, l.RunsDir(), runID); got != eventsBefore {
		t.Fatalf("run journal grew from %d to %d events — something in this process wrote to a run the engine owns", eventsBefore, got)
	}
	rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	if phase, err := rd.Phase(); err != nil || phase != journal.PhaseRunning {
		t.Fatalf("run phase = %q (err %v), want running — the daemon must not terminalize a run the engine still drives", phase, err)
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var sawReattached bool
	for _, ev := range events {
		if ev.Type != journal.EventRunnerAnnotation || ev.RunID != runID || ev.Runner == nil {
			continue
		}
		if ev.Runner["kind"] != journal.RunnerAnnotationRunRecovery {
			continue
		}
		switch ev.Runner["action"] {
		case journal.RecoveryActionResumed:
			t.Fatalf("instance log carries a run.recovery annotation with action=resumed for engine-driven run %s: %+v", runID, ev.Runner)
		case journal.RecoveryActionReattached:
			sawReattached = true
			if ev.Runner["driver"] != string(journal.DriverEngine) {
				t.Errorf("re-attachment annotation driver = %v, want %q", ev.Runner["driver"], journal.DriverEngine)
			}
		}
	}
	if !sawReattached {
		t.Fatalf("instance log has no run.recovery annotation with action=%s for %s", journal.RecoveryActionReattached, runID)
	}

	// And the daemon really did attach: describe, then await.
	deadline := time.Now().Add(5 * time.Second)
	for {
		described, awaited, _ := fake.snapshot()
		if len(described) == 1 && described[0] == runID && len(awaited) == 1 && awaited[0] == runID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine calls = described %v awaited %v, want a describe and an await for %s", described, awaited, runID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestResumeScanStillResumesRunnerDrivenRun is the type-1/type-2 negative
// control: a run.yaml with no driver field — every run any released Goobers
// has ever written — takes the pre-existing path unchanged.
func TestResumeScanStillResumesRunnerDrivenRun(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "runner-driven-interrupted"
	createDriverRun(t, l.RunsDir(), runID, "default-implement", "example", "", time.Now(), nil)

	ctx := context.Background()
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(ctx, l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = setup.Shutdown(context.Background()) }()
	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	if err := sched.Reconcile(l.RunsDir(), time.Now()); err != nil {
		t.Fatal(err)
	}

	fake := &fakeEngineWorkflows{}
	resumed, warned, reattached, err := resumeInterruptedRunsWithRunners(
		ctx, l, setup.Runners, setup.LegacyRunner, setup.RunnerRegistry, &engineRunGuards{client: fake},
		setup.Machines, setup.GooberDigests, setup.RepoRefs, setup.InstanceLog,
		setup.Telemetry, setup.RollupDB, setup.Watermarks, sched.ReleaseReconciled, &wg,
	)
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if len(resumed) != 1 || resumed[0] != runID {
		t.Fatalf("resumed = %v (warned %v), want [%s] — a driverless run.yaml is a runner-driven run", resumed, warned, runID)
	}
	if len(reattached) != 0 {
		t.Fatalf("reattached = %v, want none", reattached)
	}
	if described, awaited, cancelled := fake.snapshot(); len(described)+len(awaited)+len(cancelled) != 0 {
		t.Fatalf("engine was contacted for a runner-driven run: described %v awaited %v cancelled %v", described, awaited, cancelled)
	}
}

// TestSweepStalledRunsCancelsEngineDrivenRun: the stall sweep's job is to
// settle a run that has gone quiet. For an engine-driven run the only writer
// that can settle it is the engine, so the sweep must call CancelWorkflow —
// the first use of it in the tree — and leave the journal alone. Writing a
// terminal event into the file while the workflow keeps executing settles
// nothing and hides the run from every subsequent sweep.
func TestSweepStalledRunsCancelsEngineDrivenRun(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{Worktrees: manager, RunsDir: layout.RunsDir()})
	if err != nil {
		t.Fatal(err)
	}
	createDriverRun(t, layout.RunsDir(), "stalled-engine-run", "implementation", "", journal.DriverEngine, now.Add(-2*time.Hour), nil)
	createDriverRun(t, layout.RunsDir(), "stalled-runner-run", "implementation", "", "", now.Add(-2*time.Hour), nil)

	fake := &fakeEngineWorkflows{}
	if err := sweepStalledRuns(
		context.Background(), layout, nil, runRunner, &engineRunGuards{client: fake}, instanceLog,
		nil, nil, nil, now, 45*time.Minute, 0,
	); err != nil {
		t.Fatal(err)
	}

	_, _, cancelled := fake.snapshot()
	if len(cancelled) != 1 || cancelled[0] != "stalled-engine-run" {
		t.Fatalf("cancelled = %v, want [stalled-engine-run] — nothing else can stop a run the engine drives", cancelled)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "stalled-engine-run", journal.PhaseRunning)
	// The runner-driven neighbour in the same sweep is terminalized exactly as
	// before: the guard is scoped to the driver, not to the sweep.
	assertWatchdogPhase(t, layout.RunsDir(), "stalled-runner-run", journal.PhaseEscalated)

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var announced bool
	for _, ev := range events {
		if ev.Type == journal.EventRunnerAnnotation && ev.RunID == "stalled-engine-run" &&
			ev.Runner != nil && ev.Runner["action"] == "engine_cancel_requested" {
			announced = true
		}
		if ev.Type == journal.EventRunFinished && ev.RunID == "stalled-engine-run" {
			t.Fatalf("instance log echoes run.finished for an engine run the daemon only asked to cancel: %+v", ev)
		}
	}
	if !announced {
		t.Fatalf("instance log has no engine_cancel_requested annotation for the stalled engine run: %+v", events)
	}
}

// TestSweepStalledRunsRefusesEngineRunWithoutEngineClient: on an instance
// with no Temporal client the sweep has no way to settle an engine-driven
// run. It must report that, not fall back to terminalizing the file — the
// fallback IS the corruption.
func TestSweepStalledRunsRefusesEngineRunWithoutEngineClient(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{Worktrees: manager, RunsDir: layout.RunsDir()})
	if err != nil {
		t.Fatal(err)
	}
	createDriverRun(t, layout.RunsDir(), "orphan-engine-run", "implementation", "", journal.DriverEngine, now.Add(-2*time.Hour), nil)

	err = sweepStalledRuns(
		context.Background(), layout, nil, runRunner, nil, nil,
		nil, nil, nil, now, 45*time.Minute, 0,
	)
	// boundedagg.Join flattens the sweep's per-entry errors into one bounded
	// message, so the assertion is on the text an operator reads in the
	// instance log rather than on the wrapped sentinel.
	if err == nil || !strings.Contains(err.Error(), "orphan-engine-run") {
		t.Fatalf("sweep error = %v, want a named refusal for the engine-driven run", err)
	}
	if !strings.Contains(err.Error(), errNoEngineClient.Error()) {
		t.Fatalf("sweep error = %v, want it to name the missing engine client", err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "orphan-engine-run", journal.PhaseRunning)
}

// TestReattachEngineRunEchoesEngineOutcome covers the bookkeeping half of a
// re-attachment in isolation: the scheduler slot is released, telemetry is
// ingested, and the instance log carries the run's terminal outcome — the
// same three things the resume path owes for a run it walks itself.
func TestReattachEngineRunEchoesEngineOutcome(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()
	createDriverRun(t, layout.RunsDir(), "attached-run", "implementation", "", journal.DriverEngine, time.Now(), nil)

	fake := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	var released []string
	reattachEngineRun(context.Background(), &engineRunGuards{client: fake}, journal.RunIdentity{
		RunID: "attached-run", Workflow: "implementation", Driver: journal.DriverEngine,
	}, engineReattachDeps{
		layout:  layout,
		log:     instanceLog,
		release: func(runID, _ string) { released = append(released, runID) },
	})

	if len(released) != 1 || released[0] != "attached-run" {
		t.Fatalf("released = %v, want the reconciled slot freed once the engine's workflow closed", released)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var echoed bool
	for _, ev := range events {
		if ev.Type == journal.EventRunFinished && ev.RunID == "attached-run" {
			echoed = true
		}
	}
	if !echoed {
		t.Fatalf("instance log has no run.finished echo for the re-attached run: %+v", events)
	}
}

// TestReattachEngineRunHoldsTheSlotWhenTheEngineCannotBeDescribed is the
// co-rollout case: `kubectl rollout restart deploy/goobers-api` while the
// Temporal frontend is also cycling. DescribeWorkflowExecution answers
// Unavailable — NOT NotFound — for a workflow that is executing perfectly
// well.
//
// Releasing the run's reconciled concurrency slot here would be irreversible
// for the life of the daemon (localscheduler.Scheduler has no periodic
// re-seed) and its wakeForDemand immediately invites the scheduler to admit a
// SECOND run of the same workflow: the duplicate-work hazard these guards
// exist to close, reached from the failure path. The slot stays reserved and
// the daemon says it does not know.
func TestReattachEngineRunHoldsTheSlotWhenTheEngineCannotBeDescribed(t *testing.T) {
	shortenEngineDescribeRetries(t)
	layout := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()
	createDriverRun(t, layout.RunsDir(), "still-running-on-engine", "implementation", "", journal.DriverEngine, time.Now(), nil)

	fake := &fakeEngineWorkflows{describeErr: serviceerror.NewUnavailable("temporal frontend restarting")}
	var released []string
	reattachEngineRun(context.Background(), &engineRunGuards{client: fake}, journal.RunIdentity{
		RunID: "still-running-on-engine", Workflow: "implementation", Driver: journal.DriverEngine,
	}, engineReattachDeps{
		layout:  layout,
		log:     instanceLog,
		release: func(runID, _ string) { released = append(released, runID) },
	})

	if len(released) != 0 {
		t.Fatalf("released = %v, want none — the daemon freed the concurrency slot of a run whose status it never established", released)
	}
	if _, awaited, _ := fake.snapshot(); len(awaited) != 0 {
		t.Fatalf("awaited = %v, want none — a describe that never succeeded cannot be followed by a wait", awaited)
	}
	// The describe was retried rather than abandoned on the first RPC error.
	if described, _, _ := fake.snapshot(); len(described) < 2 {
		t.Fatalf("described = %v, want more than one attempt — a single Unavailable must not end the attachment", described)
	}

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, ev := range events {
		if ev.Type == journal.EventRunFinished && ev.RunID == "still-running-on-engine" {
			t.Fatalf("daemon echoed a terminal for a run it could not describe: %+v", ev)
		}
		if ev.Type == journal.EventError && ev.RunID == "still-running-on-engine" && ev.Error != nil &&
			ev.Error.Code == "engine_run_reattach_failed" {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("instance log has no engine_run_reattach_failed report: %+v", events)
	}
}

// TestReattachEngineRunEchoesARealTerminalPhaseForAFailedWorkflow: a workflow
// that ends badly reports its failure through Get, and every non-Completed
// terminal takes that arm. The echo has to carry a journal.RunPhase —
// readmodel's projection drops a run.finished whose status is not one (its
// terminalPhase guard), which would make the echo inert in exactly the cases
// an operator most wants it.
func TestReattachEngineRunEchoesARealTerminalPhaseForAFailedWorkflow(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()
	createDriverRun(t, layout.RunsDir(), "failed-on-engine", "implementation", "", journal.DriverEngine, time.Now(), nil)

	// Running at describe time, then reporting its own failure through the
	// wait — the shape a daemon that re-attached mid-run observes.
	fake := &fakeEngineWorkflows{
		status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		getErr: temporal.NewApplicationError("stage implement failed", "stage_failed"),
	}
	var released []string
	reattachEngineRun(context.Background(), &engineRunGuards{client: fake}, journal.RunIdentity{
		RunID: "failed-on-engine", Workflow: "implementation", Driver: journal.DriverEngine,
	}, engineReattachDeps{
		layout:  layout,
		log:     instanceLog,
		release: func(runID, _ string) { released = append(released, runID) },
	})

	if len(released) != 1 {
		t.Fatalf("released = %v, want the slot freed — the workflow reported its own outcome, so the run is over", released)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var echoed *journal.Event
	for i := range events {
		if events[i].Type == journal.EventRunFinished && events[i].RunID == "failed-on-engine" {
			echoed = &events[i]
		}
	}
	if echoed == nil {
		t.Fatalf("instance log has no run.finished echo for the failed engine run: %+v", events)
	}
	if !isTerminalPhase(journal.RunPhase(echoed.Status)) {
		t.Fatalf("echo status = %q, which is not a journal.RunPhase — readmodel's projection drops it and the run never leaves running",
			echoed.Status)
	}
	if !strings.Contains(echoed.Reason, "stage implement failed") {
		t.Fatalf("echo reason = %q, want the workflow's own failure text", echoed.Reason)
	}
}

// shortenEngineDescribeRetries collapses the re-attachment's transient-describe
// retry budget so a test can reach the give-up path without waiting minutes.
func shortenEngineDescribeRetries(t *testing.T) {
	t.Helper()
	budget, initial, max := engineDescribeRetryBudget, engineDescribeRetryInitial, engineDescribeRetryMax
	engineDescribeRetryBudget = 30 * time.Millisecond
	engineDescribeRetryInitial = time.Millisecond
	engineDescribeRetryMax = 5 * time.Millisecond
	t.Cleanup(func() {
		engineDescribeRetryBudget, engineDescribeRetryInitial, engineDescribeRetryMax = budget, initial, max
	})
}

// TestReattachEngineRunReportsUnresolvableWorkflow: a run the engine has no
// record of (history retention expired, or a scheduled run whose workflow id
// is not its run id) is reported, never driven and never terminalized.
func TestReattachEngineRunReportsUnresolvableWorkflow(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()

	fake := &fakeEngineWorkflows{notFound: true}
	reattachEngineRun(context.Background(), &engineRunGuards{client: fake}, journal.RunIdentity{
		RunID: "vanished-run", Workflow: "implementation", Driver: journal.DriverEngine,
	}, engineReattachDeps{layout: layout, log: instanceLog})

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, ev := range events {
		if ev.Type == journal.EventRunFinished && ev.RunID == "vanished-run" {
			t.Fatalf("daemon echoed a terminal outcome it never observed: %+v", ev)
		}
		if ev.Type == journal.EventError && ev.RunID == "vanished-run" && ev.Error != nil &&
			ev.Error.Code == "engine_run_unresolvable" {
			reported = true
			// The recovery vocabulary an operator's log scraper reads must be
			// complete on the way out as well as the way in: a documented
			// action nothing ever writes is a contract that never fires.
			if ev.Runner["action"] != journal.RecoveryActionUnresolved {
				t.Errorf("unresolvable report action = %v, want %q", ev.Runner["action"], journal.RecoveryActionUnresolved)
			}
		}
	}
	if !reported {
		t.Fatalf("instance log has no engine_run_unresolvable report: %+v", events)
	}
}

// TestRunAbortRefusesEngineDrivenRun: `run abort` appends a terminal event
// straight into a run's own journal. On an engine-driven run that forges a
// terminal for a workflow that keeps executing and keeps emitting into the
// same file.
func TestRunAbortRefusesEngineDrivenRun(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	const runID = "engine-run-abort"
	createDriverRun(t, l.RunsDir(), runID, "default-implement", "", journal.DriverEngine, time.Now(), nil)
	before := runEventCount(t, l.RunsDir(), runID)

	code, _, stderr := runArgs(t, "run", "abort", runID, root)
	if code != 1 {
		t.Fatalf("run abort: code = %d, stderr = %q, want the business refusal (1)", code, stderr)
	}
	if !strings.Contains(stderr, "engine-driven") || !strings.Contains(stderr, runID) {
		t.Fatalf("run abort stderr = %q, want a named engine-driven refusal", stderr)
	}
	if got := runEventCount(t, l.RunsDir(), runID); got != before {
		t.Fatalf("run journal grew from %d to %d events — abort wrote into a journal the engine owns", before, got)
	}
	assertWatchdogPhase(t, l.RunsDir(), runID, journal.PhaseRunning)
}

// TestRunAbortOnTerminalEngineDrivenRunReportsItTerminal: the engine-driven
// refusal is about protecting a journal that still has a writer. Once the run
// is closed there is nothing left to protect, and the refusal's advice — go
// act on the engine's workflow — points the operator at a workflow that has
// finished. The accurate answer is the command's own terminal guard.
func TestRunAbortOnTerminalEngineDrivenRunReportsItTerminal(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	const runID = "engine-run-abort-terminal"
	createDriverRun(t, l.RunsDir(), runID, "default-implement", "", journal.DriverEngine, time.Now(), nil)
	closeDriverRun(t, l.RunsDir(), runID, journal.PhaseCompleted)

	code, _, stderr := runArgs(t, "run", "abort", runID, root)
	if code != 1 {
		t.Fatalf("run abort: code = %d, stderr = %q, want the business refusal (1)", code, stderr)
	}
	if !strings.Contains(stderr, "already terminal") {
		t.Fatalf("run abort stderr = %q, want the already-terminal answer", stderr)
	}
	if strings.Contains(stderr, "engine-driven") {
		t.Fatalf("run abort stderr = %q, want it NOT to send the operator after a workflow that already finished", stderr)
	}
}

// closeDriverRun appends a terminal event to a run created by createDriverRun.
func closeDriverRun(t *testing.T, runsDir, runID string, phase journal.RunPhase) {
	t.Helper()
	run, _, err := journal.Recover(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRunCancelRefusesEngineDrivenRun: `run cancel` asks the daemon to stop a
// run it is executing in-process. It never is, for an engine-driven run — and
// the generic "not currently running under this daemon" answer reads like a
// race, which invites the operator to reach for `run abort` instead.
func TestRunCancelRefusesEngineDrivenRun(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	const runID = "engine-run-cancel"
	createDriverRun(t, l.RunsDir(), runID, "default-implement", "", journal.DriverEngine, time.Now(), nil)

	code, _, stderr := runArgs(t, "run", "cancel", runID, root)
	if code != 1 {
		t.Fatalf("run cancel: code = %d, stderr = %q, want the business refusal (1)", code, stderr)
	}
	if !strings.Contains(stderr, "engine-driven") || !strings.Contains(stderr, runID) {
		t.Fatalf("run cancel stderr = %q, want a named engine-driven refusal", stderr)
	}
}

// TestInterventionRefusesEngineDrivenRun: every intervention this service
// performs runs through resolve() and then either Runner.Resume /
// ResumeFromTerminal or a direct journal append. All of them are wrong for a
// run the engine drives, so the refusal sits at resolve() where they share a
// seam.
func TestInterventionRefusesEngineDrivenRun(t *testing.T) {
	machine := interventionTerminalTestMachine(t, apiv1.EvaluatorAgentic)
	const runID = "engine-run-intervention"
	service, runDir := newInterventionServiceTestRun(t, machine, runID, []journal.Event{
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "escalate"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	// Stamp the driver into the run.yaml the service reads. run.yaml is a
	// single YAML map, so appending the key is the same bytes journal.Create
	// would have written for an engine-authored run.
	markRunYAMLEngineDriven(t, runDir)

	_, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "operator", Decision: "pass",
		Rationale: "operator accepted the outcome",
	})
	if err == nil {
		t.Fatal("approve on an engine-driven run succeeded; it must be refused")
	}
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) {
		t.Fatalf("approve error = %v (%T), want an httpapi.InterventionError", err, err)
	}
	if interventionErr.Code != "run_engine_driven" {
		t.Fatalf("approve error code = %q, want run_engine_driven", interventionErr.Code)
	}
	if !strings.Contains(interventionErr.Error(), runID) {
		t.Fatalf("approve error = %q, want it to name the run", interventionErr.Error())
	}
}

func markRunYAMLEngineDriven(t *testing.T, runDir string) {
	t.Helper()
	path := filepath.Join(runDir, "run.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("driver: engine\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if !id.EngineDriven() {
		t.Fatalf("fixture run.yaml is not engine-driven after stamping: %+v", id)
	}
}
