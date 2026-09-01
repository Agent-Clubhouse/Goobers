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

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	schedulepb "go.temporal.io/api/schedule/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// enginerunid_test.go is decision 005 D2 (#3877) at the daemon and CLI seams:
// a scheduled engine run's RunID is REWRITTEN (RunScheduled hashes the
// Schedule claim workflow's id into it), so every guard that addresses a run
// by its own id gets NotFound for a run that is executing perfectly well —
// and NotFound is treated as SETTLED.
//
// Settlement releases the scheduler's reconciled concurrency slot, which
// invites a second run of the same workflow: the duplicate-driver hazard the
// guards exist to prevent, reached from the recovery path. Decision 003 named
// this the engine_run_unresolvable case and accepted it; decision 005 ruling 5
// makes it must-fix.
//
// Each test drives the real function over a real run directory, against a fake
// Temporal client, so the assertions are about the daemon's and the CLI's
// behaviour rather than about a Temporal server.

const (
	// The shape a Schedule fire produces: the claim workflow's id, the "-run"
	// child that actually executes, and the hash the run's journal is keyed
	// under.
	testScheduleClaimID = "goobers-aaaa-bbbb-2026-08-30T02:00:00Z"
	testScheduleChildID = testScheduleClaimID + "-run"
)

func scheduledRunID() string { return engine.RunID(testScheduleClaimID) }

// engineWorkflowIDResolverFor builds the resolver the daemon installs at boot,
// over a fake open-workflow enumeration, so a test exercises the real
// engine.WorkflowLiveness inverse rather than a hand-written map.
func engineWorkflowIDResolverFor(lister *fakeOpenWorkflowLister, gaggles ...string) func(context.Context, string) (string, error) {
	owned := make(map[string]struct{}, len(gaggles))
	for _, gaggle := range gaggles {
		owned[gaggle] = struct{}{}
	}
	liveness := engine.NewWorkflowLiveness(lister, "default")
	return func(ctx context.Context, runID string) (string, error) {
		return liveness.ResolveWorkflowID(ctx, runID, owned)
	}
}

// fakeOpenWorkflowLister is the visibility half of the fake engine: it lists
// open workflows with the memo fields the inverse filters on.
type fakeOpenWorkflowLister struct {
	mu     sync.Mutex
	open   map[string]string // workflow id -> gaggle
	err    error
	pages  int
	listed int
}

func (f *fakeOpenWorkflowLister) DescribeWorkflowExecution(_ context.Context, workflowID, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.open[workflowID]; !ok {
		return nil, serviceerror.NewNotFound("workflow not found")
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING},
	}, nil
}

func (f *fakeOpenWorkflowLister) ListWorkflow(_ context.Context, _ *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
	if f.err != nil {
		return nil, f.err
	}
	if f.pages > 0 {
		return &workflowservice.ListWorkflowExecutionsResponse{NextPageToken: []byte("more")}, nil
	}
	resp := &workflowservice.ListWorkflowExecutionsResponse{}
	for workflowID, gaggle := range f.open {
		info := &workflowpb.WorkflowExecutionInfo{
			Execution: &commonpb.WorkflowExecution{WorkflowId: workflowID},
		}
		payload, err := converter.GetDefaultDataConverter().ToPayload(gaggle)
		if err != nil {
			return nil, err
		}
		info.Memo = &commonpb.Memo{Fields: map[string]*commonpb.Payload{engine.RunGaggleMemoKey: payload}}
		resp.Executions = append(resp.Executions, info)
	}
	return resp, nil
}

// TestReattachResolvesARewrittenRunIDAndEchoesItsOutcome is the restart story
// this issue exists for.
//
// A daemon restart finds runs/<hash>/ with `driver: engine`, describes the
// hash, and gets NotFound because the workflow executing that run is the
// Schedule child. Before #3877 that was settlement: the slot was released and
// the instance log recorded engine_run_unresolvable for a live run. Now the
// NotFound is resolved through the open-workflow inverse, the daemon waits on
// the real workflow, and the run gets a real run.finished echo.
func TestReattachResolvesARewrittenRunIDAndEchoesItsOutcome(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()
	runID := scheduledRunID()
	createDriverRun(t, layout.RunsDir(), runID, "implementation", "web", journal.DriverEngine, time.Now(), nil)

	fake := &fakeEngineWorkflows{
		status:      enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		workflowIDs: map[string]string{runID: testScheduleChildID},
	}
	lister := &fakeOpenWorkflowLister{open: map[string]string{
		testScheduleClaimID: "web",
		testScheduleChildID: "web",
	}}
	guards := (&engineRunGuards{client: fake}).
		withWorkflowIDResolver(engineWorkflowIDResolverFor(lister, "web"))

	var released []string
	reattachEngineRun(context.Background(), guards, journal.RunIdentity{
		RunID: runID, Workflow: "implementation", Gaggle: "web", Driver: journal.DriverEngine,
	}, engineReattachDeps{
		layout: layout, log: instanceLog,
		release: func(id, _ string) { released = append(released, id) },
	})

	// The daemon addressed the CHILD workflow, which is the one executing the
	// run — never the claim, which cannot be waited on for the run's result.
	described, awaited, _ := fake.snapshot()
	if len(described) != 2 || described[0] != runID || described[1] != testScheduleChildID {
		t.Fatalf("described %v, want the run id first (the free direct-run path) then the resolved child %q",
			described, testScheduleChildID)
	}
	if len(awaited) != 1 || awaited[0] != testScheduleChildID {
		t.Fatalf("awaited %v, want the resolved child workflow %q", awaited, testScheduleChildID)
	}

	events := instanceLogEvents(t, layout)
	for _, ev := range events {
		if ev.Error != nil && ev.Error.Code == "engine_run_unresolvable" {
			t.Fatalf("a live scheduled run was reported unresolvable: %+v", ev)
		}
	}
	var echoed bool
	for _, ev := range events {
		if ev.Type == journal.EventRunFinished && ev.RunID == runID {
			echoed = true
		}
	}
	if !echoed {
		t.Fatalf("instance log has no run.finished echo for the reattached scheduled run: %+v", events)
	}
	if len(released) != 1 || released[0] != runID {
		t.Fatalf("released = %v, want the slot released exactly once, after the workflow closed", released)
	}
	// And the run's own journal is where the engine left it: the engine is
	// that file's only writer, on the way in and on the way out.
	assertWatchdogPhase(t, layout.RunsDir(), runID, journal.PhaseRunning)
}

// TestReattachHoldsTheSlotWhenTheInverseCannotAnswer: an enumeration that
// could not complete — visibility down, or the page cap exceeded — is UNKNOWN,
// not "not open". Treating unknown as settlement is precisely the release that
// frees a live run's slot, and it is the failure mode the bounded scan makes
// REACHABLE (finding 002's scan-budget risk), so it has to be proven.
func TestReattachHoldsTheSlotWhenTheInverseCannotAnswer(t *testing.T) {
	for name, lister := range map[string]*fakeOpenWorkflowLister{
		"visibility unavailable": {err: errors.New("visibility store unavailable")},
		"page cap exceeded":      {pages: 1},
		"ambiguous run id": {open: map[string]string{
			// Two unrelated open workflows claiming one run id: the scheduled
			// child, and a direct run whose id IS that hash.
			testScheduleChildID:               "web",
			engine.RunID(testScheduleClaimID): "web",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = instanceLog.Close() }()
			runID := scheduledRunID()

			guards := (&engineRunGuards{client: &fakeEngineWorkflows{notFound: true}}).
				withWorkflowIDResolver(engineWorkflowIDResolverFor(lister, "web"))
			var released []string
			reattachEngineRun(context.Background(), guards, journal.RunIdentity{
				RunID: runID, Workflow: "implementation", Gaggle: "web", Driver: journal.DriverEngine,
			}, engineReattachDeps{
				layout: layout, log: instanceLog,
				release: func(id, _ string) { released = append(released, id) },
			})

			if len(released) != 0 {
				t.Fatalf("released %v on an UNKNOWN resolution; a slot freed under a live workflow invites a second driver", released)
			}
			var reported bool
			for _, ev := range instanceLogEvents(t, layout) {
				if ev.Error != nil && ev.Error.Code == "engine_run_reattach_failed" {
					reported = true
				}
				if ev.Type == journal.EventRunFinished && ev.RunID == runID {
					t.Fatalf("daemon echoed a terminal outcome it never observed: %+v", ev)
				}
			}
			if !reported {
				t.Fatal("an unresolvable-for-unknown-reasons reattach was not reported as a failure")
			}
		})
	}
}

// TestReattachStillSettlesARunNothingIsExecuting: the inverse must not turn
// every NotFound into an indefinite hold. A run no open workflow maps to IS
// over — nothing on the engine can be driving it — and holding its slot
// forever would turn one vanished run into a permanent concurrency outage for
// its workflow.
func TestReattachStillSettlesARunNothingIsExecuting(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()

	guards := (&engineRunGuards{client: &fakeEngineWorkflows{notFound: true}}).
		withWorkflowIDResolver(engineWorkflowIDResolverFor(
			&fakeOpenWorkflowLister{open: map[string]string{"someone-elses-workflow": "web"}}, "web"))
	var released []string
	reattachEngineRun(context.Background(), guards, journal.RunIdentity{
		RunID: "vanished-run", Workflow: "implementation", Gaggle: "web", Driver: journal.DriverEngine,
	}, engineReattachDeps{
		layout: layout, log: instanceLog,
		release: func(id, _ string) { released = append(released, id) },
	})

	if len(released) != 1 {
		t.Fatalf("released = %v, want the slot released for a run nothing is executing", released)
	}
	var reported bool
	for _, ev := range instanceLogEvents(t, layout) {
		if ev.Error != nil && ev.Error.Code == "engine_run_unresolvable" {
			reported = true
		}
	}
	if !reported {
		t.Fatal("a genuinely unresolvable run was not reported")
	}
}

// TestSweepStalledRunsCancelsARewrittenRunID: the stall sweep's cancel takes
// the same NotFound resolution as the reattach describe. Cancelling by a
// scheduled run's own id addresses nothing, and a cancel that cancelled
// nothing must never read as one that landed.
func TestSweepStalledRunsCancelsARewrittenRunID(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()
	runID := scheduledRunID()
	createDriverRun(t, layout.RunsDir(), runID, "implementation", "", journal.DriverEngine, now.Add(-2*time.Hour), nil)

	fake := &fakeEngineWorkflows{workflowIDs: map[string]string{runID: testScheduleChildID}}
	guards := (&engineRunGuards{client: fake}).withWorkflowIDResolver(engineWorkflowIDResolverFor(
		&fakeOpenWorkflowLister{open: map[string]string{testScheduleChildID: ""}}, ""))

	if err := sweepStalledRuns(
		context.Background(), layout, nil, nil, guards, instanceLog,
		nil, nil, nil, now, 45*time.Minute, 0,
	); err != nil {
		t.Fatal(err)
	}
	_, _, cancelled := fake.snapshot()
	if len(cancelled) != 2 || cancelled[0] != runID || cancelled[1] != testScheduleChildID {
		t.Fatalf("cancelled %v, want the run id attempted first then the resolved child %q", cancelled, testScheduleChildID)
	}
	assertWatchdogPhase(t, layout.RunsDir(), runID, journal.PhaseRunning)
}

// TestSweepStalledRunsReportsAnEngineRunItCannotName: a cancel it could not
// address is reported and retried next tick — NEVER downgraded to
// terminalizing the journal file. The fallback IS the corruption.
func TestSweepStalledRunsReportsAnEngineRunItCannotName(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	runID := scheduledRunID()
	createDriverRun(t, layout.RunsDir(), runID, "implementation", "", journal.DriverEngine, now.Add(-2*time.Hour), nil)

	guards := (&engineRunGuards{client: &fakeEngineWorkflows{cancelErr: serviceerror.NewNotFound("workflow not found")}}).
		withWorkflowIDResolver(engineWorkflowIDResolverFor(&fakeOpenWorkflowLister{}, ""))
	err := sweepStalledRuns(
		context.Background(), layout, nil, nil, guards, nil,
		nil, nil, nil, now, 45*time.Minute, 0,
	)
	if err == nil || !strings.Contains(err.Error(), runID) {
		t.Fatalf("sweep error = %v, want a named failure for the run it could not cancel", err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), runID, journal.PhaseRunning)
}

// engineCLIFixture stands up an engine-configured instance whose single dial
// seam yields a fake Temporal client, which is the whole surface `run cancel`
// and `run abort` reach the engine through.
type engineCLIFixture struct {
	root   string
	layout instance.Layout
	engine *fakeEngineWorkflows
	lister *fakeOpenWorkflowLister
}

func newEngineCLIFixture(t *testing.T, open map[string]string) *engineCLIFixture {
	t.Helper()
	root := initDemo(t)
	configureEngineInstance(t, root)
	fixture := &engineCLIFixture{
		root:   root,
		layout: instance.NewLayout(root),
		lister: &fakeOpenWorkflowLister{open: open},
	}
	fixture.engine = &fakeEngineWorkflows{workflowIDs: map[string]string{}}
	for workflowID := range open {
		fixture.engine.workflowIDs[workflowID] = workflowID
	}
	previousDial := dialDaemonEngine
	dialDaemonEngine = func(string, string) (client.Client, error) {
		return &fakeTemporalClient{workflows: fixture.engine, lister: fixture.lister}, nil
	}
	t.Cleanup(func() { dialDaemonEngine = previousDial })
	return fixture
}

// fakeTemporalClient is a client.Client that answers only the calls the
// operator cancel path makes, and inherits panics for everything else — so a
// change that starts using the connection for something new has to say so
// here.
type fakeTemporalClient struct {
	client.Client
	workflows *fakeEngineWorkflows
	lister    *fakeOpenWorkflowLister
}

func (c *fakeTemporalClient) CancelWorkflow(ctx context.Context, workflowID, runID string) error {
	if err := c.workflows.CancelWorkflow(ctx, workflowID, runID); err != nil {
		return err
	}
	c.lister.mu.Lock()
	defer c.lister.mu.Unlock()
	if _, open := c.lister.open[workflowID]; !open {
		return serviceerror.NewNotFound("workflow not found")
	}
	return nil
}

func (c *fakeTemporalClient) ListWorkflow(ctx context.Context, req *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	return c.lister.ListWorkflow(ctx, req)
}

func (c *fakeTemporalClient) WorkflowService() workflowservice.WorkflowServiceClient { return nil }

func (c *fakeTemporalClient) Close() {}

// TestRunCancelRoutesAnEngineRunToCancelWorkflow is decision 003 ruling 5,
// carried into decision 005: `goobers run cancel` on an engine-driven run
// stops being a refusal and becomes a CancelWorkflow.
//
// The refusal was right about the hazard and useless as an answer: it told the
// operator to "cancel that workflow", which for a SCHEDULED run means deriving
// a workflow id they cannot derive. The cancel must therefore resolve the
// rewritten run id, address the real workflow, leave the run's journal
// untouched, and record what it did where the sweep's own cancellations are
// recorded.
func TestRunCancelRoutesAnEngineRunToCancelWorkflow(t *testing.T) {
	fixture := newEngineCLIFixture(t, map[string]string{testScheduleChildID: ""})
	runID := scheduledRunID()
	createDriverRun(t, fixture.layout.RunsDir(), runID, "default-implement", "", journal.DriverEngine, time.Now(), nil)
	before := runEventCount(t, fixture.layout.RunsDir(), runID)

	code, stdout, stderr := runArgs(t, "run", "cancel", runID, fixture.root)
	if code != 0 {
		t.Fatalf("run cancel: code = %d, stderr = %q, want 0", code, stderr)
	}
	_, _, cancelled := fixture.engine.snapshot()
	if len(cancelled) != 2 || cancelled[1] != testScheduleChildID {
		t.Fatalf("cancelled %v, want the rewritten run id resolved to the child workflow %q", cancelled, testScheduleChildID)
	}
	if !strings.Contains(stdout, testScheduleChildID) {
		t.Errorf("run cancel stdout = %q, want it to name the Temporal workflow the operator can go confirm", stdout)
	}

	// The run's journal is untouched and still running: the engine writes its
	// terminal event once the cancellation lands.
	if got := runEventCount(t, fixture.layout.RunsDir(), runID); got != before {
		t.Fatalf("run journal grew from %d to %d events — cancel wrote into a journal the engine owns", before, got)
	}
	assertWatchdogPhase(t, fixture.layout.RunsDir(), runID, journal.PhaseRunning)

	var announced bool
	for _, ev := range instanceLogEvents(t, fixture.layout) {
		if ev.Type == journal.EventRunnerAnnotation && ev.RunID == runID && ev.Runner != nil &&
			ev.Runner["action"] == journal.RecoveryActionEngineCancelRequested {
			announced = true
			if got, _ := ev.Runner["workflowId"].(string); got != testScheduleChildID {
				t.Errorf("annotation workflowId = %q, want %q", got, testScheduleChildID)
			}
		}
		if ev.Type == journal.EventRunFinished && ev.RunID == runID {
			t.Fatalf("instance log echoes run.finished for a run the CLI only asked to cancel: %+v", ev)
		}
	}
	if !announced {
		t.Fatal("instance log has no engine_cancel_requested annotation for the cancelled engine run")
	}
}

// TestRunAbortRoutesAnEngineRunToCancelWorkflow: abort takes the identical
// path. It is the daemon-DOWN repair command, and no daemon runs in this test
// — which is the point: an engine run is not one the daemon executes, so
// requiring a live daemon would leave it unstoppable during exactly the outage
// an operator most wants to stop it in.
func TestRunAbortRoutesAnEngineRunToCancelWorkflow(t *testing.T) {
	const directID = "engine-run-abort-direct"
	fixture := newEngineCLIFixture(t, map[string]string{directID: ""})
	createDriverRun(t, fixture.layout.RunsDir(), directID, "default-implement", "", journal.DriverEngine, time.Now(), nil)
	before := runEventCount(t, fixture.layout.RunsDir(), directID)

	code, stdout, stderr := runArgs(t, "run", "abort", directID, fixture.root)
	if code != 0 {
		t.Fatalf("run abort: code = %d, stderr = %q, want 0", code, stderr)
	}
	// A DIRECT run's workflow id IS its run id, so the cancel lands on the
	// first attempt and the open-workflow inverse is never paged.
	_, _, cancelled := fixture.engine.snapshot()
	if len(cancelled) != 1 || cancelled[0] != directID {
		t.Fatalf("cancelled %v, want exactly the run id itself", cancelled)
	}
	if fixture.lister.listed != 1 {
		t.Errorf("open-workflow scans = %d, want only the boot enumeration — a direct run's cancel resolves nothing",
			fixture.lister.listed)
	}
	if !strings.Contains(stdout, directID) {
		t.Errorf("run abort stdout = %q, want it to name the run", stdout)
	}
	if got := runEventCount(t, fixture.layout.RunsDir(), directID); got != before {
		t.Fatalf("run journal grew from %d to %d events — abort forged a terminal into a journal the engine owns", before, got)
	}
	assertWatchdogPhase(t, fixture.layout.RunsDir(), directID, journal.PhaseRunning)
}

// TestRunCancelRefusesAnEngineRunItCannotName: a cancel that cannot address a
// workflow fails loudly and leaves the journal alone. Downgrading it to
// terminalizing the file is the corruption both commands exist to avoid, and
// reporting success for a cancel that cancelled nothing is worse than either.
func TestRunCancelRefusesAnEngineRunItCannotName(t *testing.T) {
	fixture := newEngineCLIFixture(t, nil)
	runID := scheduledRunID()
	createDriverRun(t, fixture.layout.RunsDir(), runID, "default-implement", "", journal.DriverEngine, time.Now(), nil)

	code, _, stderr := runArgs(t, "run", "cancel", runID, fixture.root)
	if code != 1 {
		t.Fatalf("run cancel: code = %d, stderr = %q, want the business refusal (1)", code, stderr)
	}
	if !strings.Contains(stderr, runID) || !strings.Contains(stderr, "no open engine workflow") {
		t.Fatalf("run cancel stderr = %q, want a named unresolvable-run failure", stderr)
	}
	assertWatchdogPhase(t, fixture.layout.RunsDir(), runID, journal.PhaseRunning)
}

// TestRunAbortRefusesAnEngineRunWithNoEngineConfigured: an engine-driven
// journal on an instance that cannot reach the engine is a misconfiguration,
// and the pre-guard behaviour — forge a terminal — is exactly the corruption
// the guards exist to prevent.
func TestRunAbortRefusesAnEngineRunWithNoEngineConfigured(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	const runID = "engine-run-abort-no-engine"
	createDriverRun(t, l.RunsDir(), runID, "default-implement", "", journal.DriverEngine, time.Now(), nil)
	before := runEventCount(t, l.RunsDir(), runID)

	code, _, stderr := runArgs(t, "run", "abort", runID, root)
	if code != 1 {
		t.Fatalf("run abort: code = %d, stderr = %q, want the business refusal (1)", code, stderr)
	}
	if !strings.Contains(stderr, errNoEngineClient.Error()) || !strings.Contains(stderr, runID) {
		t.Fatalf("run abort stderr = %q, want it to name the run and the missing engine client", stderr)
	}
	if got := runEventCount(t, l.RunsDir(), runID); got != before {
		t.Fatalf("run journal grew from %d to %d events", before, got)
	}
	assertWatchdogPhase(t, l.RunsDir(), runID, journal.PhaseRunning)
}

// fakeScheduleLister answers the boot invariant's ListSchedules.
type fakeScheduleLister struct {
	ids   []string
	err   error
	pages int
	calls int
}

func (f *fakeScheduleLister) ListSchedules(_ context.Context, _ *workflowservice.ListSchedulesRequest, _ ...grpc.CallOption) (*workflowservice.ListSchedulesResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.pages > 0 {
		return &workflowservice.ListSchedulesResponse{NextPageToken: []byte("more")}, nil
	}
	resp := &workflowservice.ListSchedulesResponse{}
	for _, id := range f.ids {
		resp.Schedules = append(resp.Schedules, &schedulepb.ScheduleListEntry{ScheduleId: id})
	}
	return resp, nil
}

// TestEngineScheduleInvariantPassesWhenNoSchedulesAreConfigured is the state
// every instance is in today: nothing in cmd/goobers constructs a
// ScheduleReconciler, so the check is a regression guard rather than a fix.
// It must therefore be silent, or an operator learns to ignore it.
func TestEngineScheduleInvariantPassesWhenNoSchedulesAreConfigured(t *testing.T) {
	lister := &fakeScheduleLister{}
	if err := assertNoEngineScheduleReconciliation(context.Background(), lister, "default"); err != nil {
		t.Fatalf("assertNoEngineScheduleReconciliation = %v, want nil on a namespace with no schedules", err)
	}
	// A namespace holding somebody else's schedules is not a violation of
	// OUR invariant: the check is scoped to the ids this tree mints.
	foreign := &fakeScheduleLister{ids: []string{"someone-elses-schedule", "cron-thing"}}
	if err := assertNoEngineScheduleReconciliation(context.Background(), foreign, "default"); err != nil {
		t.Fatalf("assertNoEngineScheduleReconciliation = %v, want nil for non-Goobers schedules", err)
	}
	// And an instance with no engine at all never even asks.
	if err := assertNoEngineScheduleReconciliation(context.Background(), nil, ""); err != nil {
		t.Fatalf("assertNoEngineScheduleReconciliation = %v, want nil with no engine configured", err)
	}
}

// TestEngineScheduleInvariantFailsOnAConfiguredSchedule: a materialized
// Schedule makes RunScheduled the start path again, which makes the bounded
// open-workflow inverse the NORMAL path for every re-attach and cancel rather
// than the exceptional one — a capacity decision nobody took. The refusal
// names the schedules, because "delete the schedule" is unactionable without
// knowing which.
func TestEngineScheduleInvariantFailsOnAConfiguredSchedule(t *testing.T) {
	scheduleID := engine.ScheduleID("instance-a", "web", "implementation", 0)
	lister := &fakeScheduleLister{ids: []string{"unrelated", scheduleID}}
	err := assertNoEngineScheduleReconciliation(context.Background(), lister, "production")
	if err == nil {
		t.Fatal("a Goobers-owned Temporal Schedule did not trip the invariant")
	}
	if errors.Is(err, errEngineScheduleCheckUnknown) {
		t.Fatalf("violation reported as unknown: %v", err)
	}
	for _, want := range []string{scheduleID, "production", "decision 005"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("invariant failure = %q, want it to name %q", err, want)
		}
	}
}

// TestEngineScheduleInvariantTreatsAFailedCheckAsUnknown: an unreachable
// frontend proves nothing either way. Refusing to boot on it would turn a
// transient Temporal outage into a daemon outage, so the two are kept apart at
// the type level rather than by reading the message.
func TestEngineScheduleInvariantTreatsAFailedCheckAsUnknown(t *testing.T) {
	for name, lister := range map[string]*fakeScheduleLister{
		"frontend unavailable": {err: errors.New("connection refused")},
		"page cap exceeded":    {pages: 1},
	} {
		t.Run(name, func(t *testing.T) {
			err := assertNoEngineScheduleReconciliation(context.Background(), lister, "production")
			if !errors.Is(err, errEngineScheduleCheckUnknown) {
				t.Fatalf("err = %v, want it to be errEngineScheduleCheckUnknown", err)
			}
		})
	}
}

// TestDaemonWiresNoTemporalScheduleReconciliation is the SOURCE-level half of
// the invariant, and the one the critic's correction rests on: "No caller of
// internal/engine/schedule*.go exists in cmd/goobers". The runtime check
// above catches a namespace that already has schedules; this catches the
// commit that would put them there.
func TestDaemonWiresNoTemporalScheduleReconciliation(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, symbol := range []string{
			"NewScheduleReconciler", "ScheduleReconciler{", "ScheduleSnapshot",
			"engine.ReconcileSchedules",
		} {
			if strings.Contains(string(source), symbol) {
				t.Errorf("cmd/goobers/%s references %s: decision 005 requires this daemon's own scheduler to be the "+
					"only trigger source for engine runs, because a Temporal Schedule fire rewrites the run's id and "+
					"makes the bounded open-workflow inverse load-bearing for every re-attach and cancel", name, symbol)
			}
		}
	}
}
