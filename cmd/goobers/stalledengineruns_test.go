package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/hostsuspend"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// stalledEngineSweepFixture is an engine run whose journal holds only
// run.started — the #5407 signature of a workflow whose first task no worker
// ever took — plus the daemon pieces the stall sweep acts through.
type stalledEngineSweepFixture struct {
	layout   instance.Layout
	log      *journal.InstanceLog
	clock    time.Time
	runID    string
	started  time.Time
	released []string
	// nothingOpen gives the guards an open-workflow scan that finds nothing.
	nothingOpen bool
	maxDuration time.Duration
	suspended   []hostsuspend.Window
}

const stalledEngineSweepTimeout = 45 * time.Minute

func newStalledEngineSweepFixture(t *testing.T) *stalledEngineSweepFixture {
	t.Helper()
	f := &stalledEngineSweepFixture{
		layout:  instance.NewLayout(t.TempDir()),
		runID:   "wedged-engine-run",
		started: time.Date(2026, 9, 18, 8, 30, 0, 0, time.UTC),
	}
	log, _, err := journal.OpenInstanceLog(f.layout.SchedulerDir(), journal.WithClock(func() time.Time { return f.clock }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	f.log = log
	writer := f.writer(t)
	reservation := liveOpenBatch(f.runID, "goobers", f.started)
	reservation.Ops = reservation.Ops[:1]
	if _, err := writer.Emit(context.Background(), reservation); err != nil {
		t.Fatalf("emit reservation: %v", err)
	}
	if closed := writer.CloseIdle(-time.Hour); len(closed) != 1 {
		t.Fatalf("CloseIdle = %v, want the reservation journal released", closed)
	}
	return f
}

// writer is a fresh live journal writer, as a restarted daemon would build.
func (f *stalledEngineSweepFixture) writer(t *testing.T) *livejournal.Writer {
	t.Helper()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return f.layout.RunsDir(), true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	return writer
}

func (f *stalledEngineSweepFixture) sweep(t *testing.T, fake *fakeEngineWorkflows, writer *livejournal.Writer, now time.Time) error {
	t.Helper()
	f.clock = now
	guards := &engineRunGuards{client: fake}
	if f.nothingOpen {
		guards = guards.withWorkflowIDResolver(engineWorkflowIDResolverFor(&fakeOpenWorkflowLister{}, ""))
	}
	return sweepStalledRuns(
		context.Background(), f.layout, nil, nil, guards, f.log,
		&stalledSweepDeps{
			CloseEngineRun:  closeTerminatedEngineRun(writer),
			HostSuspensions: hostsuspend.NewLedger(nil, f.suspended),
		}, nil,
		func(runID, _ string) { f.released = append(f.released, runID) },
		now, stalledEngineSweepTimeout, f.maxDuration,
	)
}

func (f *stalledEngineSweepFixture) recoveryActions(t *testing.T) []string {
	t.Helper()
	events, err := journal.ReadInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, ev := range events {
		if ev.RunID == f.runID && ev.Type == journal.EventRunnerAnnotation && ev.Runner != nil {
			if action, ok := ev.Runner["action"].(string); ok {
				actions = append(actions, action)
			}
		}
	}
	return actions
}

// TestSweepStalledRunsTerminatesEngineRunWhoseCancelIsNeverHonoured is #5407:
// with no worker able to poll, the stall sweep's CancelWorkflow sits on the
// server forever and the run holds its slot across every restart. The sweep
// must keep the slot on the cancellation request, then — once a further full
// stall timeout has passed since that request, dated from the instance log so
// a restart in between changes nothing — terminate the workflow server-side,
// close the journal and only then release the slot.
func TestSweepStalledRunsTerminatesEngineRunWhoseCancelIsNeverHonoured(t *testing.T) {
	f := newStalledEngineSweepFixture(t)

	// First seen long after it went silent (the daemon was down): the run is
	// cancelled, never terminated, however old its silence is.
	firstSeen := f.started.Add(4 * stalledEngineSweepTimeout)
	firstDaemon := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	if err := f.sweep(t, firstDaemon, f.writer(t), firstSeen); err != nil {
		t.Fatal(err)
	}
	if _, _, cancelled := firstDaemon.snapshot(); len(cancelled) != 1 || cancelled[0] != f.runID {
		t.Fatalf("cancelled = %v, want the stalled run cancelled first", cancelled)
	}
	if len(firstDaemon.terminated) != 0 {
		t.Fatalf("terminated = %v before any cancellation was requested, want only a cancellation request", firstDaemon.terminated)
	}
	if err := f.sweep(t, firstDaemon, f.writer(t), firstSeen.Add(stalledEngineSweepTimeout-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(firstDaemon.terminated) != 0 || len(f.released) != 0 {
		t.Fatalf("terminated %v released %v within a timeout of the cancellation, want the slot held", firstDaemon.terminated, f.released)
	}
	assertWatchdogPhase(t, f.layout.RunsDir(), f.runID, journal.PhaseRunning)

	// The daemon restarts. No worker ever took the cancellation, so the
	// workflow is still open and the journal still holds only run.started.
	restarted := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	now := firstSeen.Add(stalledEngineSweepTimeout + time.Minute)
	if err := f.sweep(t, restarted, f.writer(t), now); err != nil {
		t.Fatal(err)
	}
	if len(restarted.terminated) != 1 || restarted.terminated[0] != f.runID {
		t.Fatalf("terminated = %v, want the unresponsive workflow terminated on the server", restarted.terminated)
	}
	if len(f.released) != 1 || f.released[0] != f.runID {
		t.Fatalf("released = %v, want the slot released once after the workflow closed", f.released)
	}
	assertWatchdogPhase(t, f.layout.RunsDir(), f.runID, journal.PhaseAborted)
	reader, err := journal.OpenRead(filepath.Join(f.layout.RunsDir(), f.runID))
	if err != nil {
		t.Fatal(err)
	}
	cause, err := reader.TerminalCause()
	if err != nil {
		t.Fatal(err)
	}
	if cause.Code != engine.TerminatedRunErrorCode || cause.Classification != journal.TerminalInfrastructureFailure ||
		cause.CausalEventSeq != 2 || !strings.Contains(cause.Message, "terminated its workflow") {
		t.Fatalf("terminal cause = %+v, want the daemon's termination recorded against the run_failed event", cause)
	}
	if actions := f.recoveryActions(t); len(actions) != 3 || actions[2] != journal.RecoveryActionEngineTerminated {
		t.Fatalf("instance-log recovery actions = %v, want two cancellation requests then the termination", actions)
	}

	// The run is closed: later ticks leave the engine alone.
	later := &fakeEngineWorkflows{}
	if err := f.sweep(t, later, f.writer(t), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if described, _, cancelled := later.snapshot(); len(described)+len(cancelled)+len(later.terminated) != 0 {
		t.Fatalf("engine contacted for a closed run: described %v cancelled %v terminated %v", described, cancelled, later.terminated)
	}
	if len(f.released) != 1 {
		t.Fatalf("released = %v, want exactly one release", f.released)
	}
}

// TestSweepStalledRunsHoldsEngineRunUntilTerminationIsConfirmed: termination
// only frees the slot once Temporal confirms the workflow closed by
// termination. A workflow that closed any other way keeps its own outcome:
// the sweep neither terminates it nor writes a terminal over its history.
func TestSweepStalledRunsHoldsEngineRunUntilTerminationIsConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fake          *fakeEngineWorkflows
		noWriter      bool
		nothingOpen   bool
		wantErr       string
		wantTerminate bool
		wantPhase     journal.RunPhase
	}{
		{
			name:          "terminate fails",
			fake:          &fakeEngineWorkflows{terminateErr: errors.New("frontend unavailable")},
			wantErr:       "frontend unavailable",
			wantTerminate: true,
			wantPhase:     journal.PhaseRunning,
		},
		{
			name:      "describe fails",
			fake:      &fakeEngineWorkflows{describeErr: errors.New("frontend unavailable")},
			wantErr:   "frontend unavailable",
			wantPhase: journal.PhaseRunning,
		},
		{
			name:      "scheduled run not found without a resolver",
			fake:      &fakeEngineWorkflows{notFound: true},
			wantErr:   errEngineRunUnresolvable.Error(),
			wantPhase: journal.PhaseRunning,
		},
		{
			name:          "no live journal writer",
			fake:          &fakeEngineWorkflows{},
			noWriter:      true,
			wantErr:       "no live journal writer",
			wantTerminate: true,
			wantPhase:     journal.PhaseRunning,
		},
		{
			name:      "workflow already terminated",
			fake:      &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED},
			wantPhase: journal.PhaseAborted,
		},
		{
			name:        "scheduled run with nothing open",
			fake:        &fakeEngineWorkflows{notFound: true},
			nothingOpen: true,
			wantPhase:   journal.PhaseRunning,
		},
		{
			name:      "workflow completed on its own",
			fake:      &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED},
			wantPhase: journal.PhaseRunning,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStalledEngineSweepFixture(t)
			if err := f.sweep(t, &fakeEngineWorkflows{}, f.writer(t), f.started.Add(stalledEngineSweepTimeout+time.Minute)); err != nil {
				t.Fatal(err)
			}
			f.nothingOpen = tc.nothingOpen
			var writer *livejournal.Writer
			if !tc.noWriter {
				writer = f.writer(t)
			}
			err := f.sweep(t, tc.fake, writer, f.started.Add(2*stalledEngineSweepTimeout+5*time.Minute))
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("sweep error = %v, want %q", err, tc.wantErr)
			}
			if got := len(tc.fake.terminated) == 1; got != tc.wantTerminate {
				t.Fatalf("terminated = %v, want terminate called = %t", tc.fake.terminated, tc.wantTerminate)
			}
			if _, _, cancelled := tc.fake.snapshot(); len(cancelled) != 0 {
				t.Fatalf("cancelled = %v past the cancellation's deadline, want escalation instead", cancelled)
			}
			assertWatchdogPhase(t, f.layout.RunsDir(), f.runID, tc.wantPhase)
			if wantRelease := tc.wantPhase == journal.PhaseAborted; (len(f.released) == 1) != wantRelease {
				t.Fatalf("released = %v, want released = %t", f.released, wantRelease)
			}
		})
	}
}

// TestSweepStalledRunsTerminatesOverAgeEngineRunOneTimeoutAfterItsCancel: a
// maximum-duration breach cancels a run that was active moments before, so the
// wait for an unanswered cancellation is dated from the cancellation itself,
// not from twice the stall timeout of journal silence.
func TestSweepStalledRunsTerminatesOverAgeEngineRunOneTimeoutAfterItsCancel(t *testing.T) {
	f := newStalledEngineSweepFixture(t)
	f.maxDuration = 2 * time.Hour
	lastActivity := f.started.Add(f.maxDuration - 5*time.Minute)
	stage := liveOpenBatch(f.runID, "goobers", lastActivity).Ops[1]
	stage.Time = lastActivity
	writer := f.writer(t)
	if _, err := writer.Emit(context.Background(), livejournal.EmitRequest{RunID: f.runID, Gaggle: "goobers", Ops: []livejournal.Op{stage}}); err != nil {
		t.Fatalf("emit stage activity: %v", err)
	}
	writer.CloseIdle(-time.Hour)

	cancelledAt := f.started.Add(f.maxDuration + time.Minute)
	first := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	if err := f.sweep(t, first, f.writer(t), cancelledAt); err != nil {
		t.Fatal(err)
	}
	if _, _, cancelled := first.snapshot(); len(cancelled) != 1 || len(first.terminated) != 0 {
		t.Fatalf("cancelled %v terminated %v, want the over-age run cancelled first", cancelled, first.terminated)
	}

	// One timeout after the cancellation the journal has been silent for
	// well under two timeouts, yet the cancellation has gone unanswered.
	second := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	if err := f.sweep(t, second, f.writer(t), cancelledAt.Add(stalledEngineSweepTimeout+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(second.terminated) != 1 || len(f.released) != 1 {
		t.Fatalf("terminated %v released %v, want the run terminated one timeout after its cancellation", second.terminated, f.released)
	}
	assertWatchdogPhase(t, f.layout.RunsDir(), f.runID, journal.PhaseAborted)
}

// TestSweepStalledRunsCreditsHostSuspensionBeforeTerminatingEngineRun is
// #5891 applied to #5407's escalation: a host asleep after the sweep asked
// the engine to cancel gave no worker a chance to take the cancellation, so
// that time is credited before the workflow is terminated, as it is for the
// stall itself.
func TestSweepStalledRunsCreditsHostSuspensionBeforeTerminatingEngineRun(t *testing.T) {
	f := newStalledEngineSweepFixture(t)
	cancelledAt := f.started.Add(stalledEngineSweepTimeout + time.Minute)
	first := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	if err := f.sweep(t, first, f.writer(t), cancelledAt); err != nil {
		t.Fatal(err)
	}
	if _, _, cancelled := first.snapshot(); len(cancelled) != 1 {
		t.Fatalf("cancelled = %v, want the stalled run cancelled first", cancelled)
	}

	f.suspended = []hostsuspend.Window{{From: cancelledAt.Add(5 * time.Minute), To: cancelledAt.Add(35 * time.Minute)}}
	asleep := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	if err := f.sweep(t, asleep, f.writer(t), cancelledAt.Add(stalledEngineSweepTimeout+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(asleep.terminated) != 0 || len(f.released) != 0 {
		t.Fatalf("terminated %v released %v while the host slept through the cancellation, want the slot held", asleep.terminated, f.released)
	}
	assertWatchdogPhase(t, f.layout.RunsDir(), f.runID, journal.PhaseRunning)

	awake := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	if err := f.sweep(t, awake, f.writer(t), cancelledAt.Add(stalledEngineSweepTimeout+31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(awake.terminated) != 1 || len(f.released) != 1 {
		t.Fatalf("terminated %v released %v, want the run terminated once a timeout of awake time passed", awake.terminated, f.released)
	}
}
