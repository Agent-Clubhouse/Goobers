package scheduling

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/hostsuspend"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// These sweep cases supply their run candidates explicitly; the filesystem
// inventory and terminal cleanup adapters remain covered in cmd/goobers.
func sweepStalledRuns(ctx context.Context, l instance.Layout, r RunnerResolver, fallback *runner.Runner, log *journal.InstanceLog, deps *StalledSweepDeps, notify runner.TerminalNotifier, release func(string, string), now time.Time, timeout, maxDuration time.Duration) error {
	entries, err := filepath.Glob(filepath.Join(l.RunsDir(), "*"))
	if err != nil {
		return err
	}
	services := StallServices{Clock: JournalActivityClock{}, Candidates: func(context.Context, instance.Layout, ...[]string) ([]string, error) { return entries, nil }}
	return services.Sweep(ctx, l, r, fallback, log, deps, notify, release, now, timeout, maxDuration)
}
func TestSweepStalledRunsUsesPinnedPerRunTimeout(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{Worktrees: manager, RunsDir: layout.RunsDir()})
	if err != nil {
		t.Fatal(err)
	}
	eventTime := now.Add(-time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "short-timeout-run", "short", "", &eventTime, time.Time{}, &apiv1.RunControls{
		MaxRepasses:       3,
		StalledRunTimeout: "30m",
	})
	eventTime = now.Add(-time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "long-timeout-run", "long", "", &eventTime, time.Time{}, &apiv1.RunControls{
		MaxRepasses:       3,
		StalledRunTimeout: "2h",
	})

	if err := sweepStalledRuns(context.Background(), layout, nil, runRunner, nil, nil, nil, nil, now, 45*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "short-timeout-run", journal.PhaseEscalated)
	assertWatchdogPhase(t, layout.RunsDir(), "long-timeout-run", journal.PhaseRunning)
}

// #5601: a run drained by a graceful `goobers down` must not be escalated as
// stalled because the daemon then stayed down longer than the stall timeout.
// Only time the daemon was up counts; a run that was already silent that long
// while the daemon was up still escalates.
func TestSweepStalledRunsCreditsGracefulDrainDowntime(t *testing.T) {
	now := time.Date(2026, 9, 24, 4, 34, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{Worktrees: manager, RunsDir: layout.RunsDir()})
	if err != nil {
		t.Fatal(err)
	}
	controls := &apiv1.RunControls{MaxRepasses: 3, StalledRunTimeout: "45m"}
	// Drained at the push-branch boundary one minute before the clean
	// shutdown; the daemon then stayed down 1h31m.
	eventTime := now.Add(-93 * time.Minute)
	createWatchdogRunWithControls(t, layout.RunsDir(), "drained-run", "implementation", "", &eventTime, time.Time{}, controls)
	// Silent for 2h of daemon uptime before the same drain.
	eventTime = now.Add(-212 * time.Minute)
	createWatchdogRunWithControls(t, layout.RunsDir(), "stalled-before-drain", "implementation", "", &eventTime, time.Time{}, controls)

	deps := &StalledSweepDeps{DrainedDowntime: CleanDaemonDowntime([]journal.Event{
		{Type: journal.EventDaemonStarted, Time: now.Add(-5 * time.Hour)},
		{Type: journal.EventDaemonCleanShutdown, Time: now.Add(-92 * time.Minute)},
		{Type: journal.EventDaemonStarted, Time: now.Add(-time.Minute)},
	})}
	if err := sweepStalledRuns(context.Background(), layout, nil, runRunner, nil, deps, nil, nil, now, 45*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "drained-run", journal.PhaseRunning)
	assertWatchdogPhase(t, layout.RunsDir(), "stalled-before-drain", journal.PhaseEscalated)

	// Without the credit (no clean shutdown on record) the drained run is
	// escalated exactly as before.
	if err := sweepStalledRuns(context.Background(), layout, nil, runRunner, nil, nil, nil, nil, now, 45*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "drained-run", journal.PhaseEscalated)
}

// #5891: a host that slept must not spend a run's maxRunDuration budget, and
// silence while it slept is not a stall. Runs that exceed either limit in
// active time are still terminated.
func TestSweepStalledRunsExcludesHostSuspension(t *testing.T) {
	now := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{Worktrees: manager, RunsDir: layout.RunsDir()})
	if err != nil {
		t.Fatal(err)
	}
	durationControls := &apiv1.RunControls{MaxRepasses: 3, StalledRunTimeout: "45m", MaxRunDuration: "2h"}
	stallControls := &apiv1.RunControls{MaxRepasses: 3, StalledRunTimeout: "45m"}
	// Three hours old with a fresh heartbeat, 90 minutes of it asleep.
	started := now.Add(-3 * time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "slept-within-budget", "implementation", "", &started, now.Add(-time.Minute), durationControls)
	// Five hours old: still over budget once the same sleep is excluded.
	started = now.Add(-5 * time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "over-budget-awake", "implementation", "", &started, now.Add(-time.Minute), durationControls)
	// Silent for two hours, 90 minutes of it asleep.
	eventTime := now.Add(-2 * time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "quiet-while-asleep", "implementation", "", &eventTime, time.Time{}, stallControls)
	// Silent for three hours: 90 minutes awake is past the stall timeout.
	eventTime = now.Add(-3 * time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "quiet-while-awake", "implementation", "", &eventTime, time.Time{}, stallControls)

	deps := &StalledSweepDeps{HostSuspensions: hostsuspend.NewLedger(nil, []hostsuspend.Window{
		{From: now.Add(-100 * time.Minute), To: now.Add(-10 * time.Minute)},
	})}
	if err := sweepStalledRuns(context.Background(), layout, nil, runRunner, nil, deps, nil, nil, now, 45*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "slept-within-budget", journal.PhaseRunning)
	assertWatchdogPhase(t, layout.RunsDir(), "over-budget-awake", journal.PhaseAborted)
	assertWatchdogPhase(t, layout.RunsDir(), "quiet-while-asleep", journal.PhaseRunning)
	assertWatchdogPhase(t, layout.RunsDir(), "quiet-while-awake", journal.PhaseEscalated)

	// Without the suspension record both remaining runs are terminated, as
	// wall-clock enforcement did before.
	if err := sweepStalledRuns(context.Background(), layout, nil, runRunner, nil, nil, nil, nil, now, 45*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "slept-within-budget", journal.PhaseAborted)
	assertWatchdogPhase(t, layout.RunsDir(), "quiet-while-asleep", journal.PhaseEscalated)
}

func TestCleanDaemonDowntimeCreditsOnlyGracefulGaps(t *testing.T) {
	base := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	windows := CleanDaemonDowntime([]journal.Event{
		{Type: journal.EventDaemonStarted, Time: at(0)},
		{Type: journal.EventDaemonCleanShutdown, Time: at(10)},
		{Type: journal.EventDaemonStarted, Time: at(40)}, // clean gap: 30m
		{Type: journal.EventDaemonDirtyRestart, Time: at(100)},
		{Type: journal.EventDaemonStarted, Time: at(100)}, // crash gap: none
		{Type: journal.EventDaemonCleanShutdown, Time: at(110)},
		{Type: journal.EventDaemonStarted, Time: at(130)}, // clean gap: 20m
	})
	want := []DaemonDowntime{{from: at(10), to: at(40)}, {from: at(110), to: at(130)}}
	if len(windows) != len(want) {
		t.Fatalf("windows = %+v, want %+v", windows, want)
	}
	for i := range want {
		if !windows[i].from.Equal(want[i].from) || !windows[i].to.Equal(want[i].to) {
			t.Fatalf("windows = %+v, want %+v", windows, want)
		}
	}
	deps := &StalledSweepDeps{DrainedDowntime: windows}
	for _, tc := range []struct {
		lastActivity time.Time
		want         time.Duration
	}{
		{at(5), 50 * time.Minute},
		{at(20), 40 * time.Minute},
		{at(115), 15 * time.Minute},
		{at(130), 0},
	} {
		if got := deps.drainedDowntimeSince(tc.lastActivity); got != tc.want {
			t.Fatalf("drainedDowntimeSince(%s) = %s, want %s", tc.lastActivity, got, tc.want)
		}
	}
	if got := (*StalledSweepDeps)(nil).drainedDowntimeSince(at(0)); got != 0 {
		t.Fatalf("nil deps credit = %s, want 0", got)
	}
}

func TestSweepStalledRunsAbortsOverAgeRunWithFreshHeartbeat(t *testing.T) {
	now := time.Date(2026, 8, 2, 20, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{Worktrees: manager, RunsDir: layout.RunsDir()})
	if err != nil {
		t.Fatal(err)
	}

	started := now.Add(-3 * time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "expired-run", "implementation", "", &started, now.Add(-time.Minute), &apiv1.RunControls{
		MaxRepasses:       3,
		StalledRunTimeout: "45m",
		MaxRunDuration:    "2h",
	})
	started = now.Add(-3 * time.Hour)
	createWatchdogRunWithControls(t, layout.RunsDir(), "duration-disabled", "implementation", "", &started, now.Add(-time.Minute), &apiv1.RunControls{
		MaxRepasses:       3,
		StalledRunTimeout: "45m",
	})

	if err := sweepStalledRuns(context.Background(), layout, nil, runRunner, nil, nil, nil, nil, now, 45*time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	assertWatchdogPhase(t, layout.RunsDir(), "expired-run", journal.PhaseAborted)
	assertWatchdogPhase(t, layout.RunsDir(), "duration-disabled", journal.PhaseRunning)

	reader, err := journal.OpenRead(filepath.Join(layout.RunsDir(), "expired-run"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[len(events)-2].Error == nil ||
		events[len(events)-2].Error.Code != runner.RunDurationExceededErrorCode {
		t.Fatalf("terminal events = %+v, want duration-exceeded error", events)
	}
}

func TestSweepStalledRunsPreservesPausedHumanGate(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	eventTime := now.Add(-2 * time.Hour)
	layout := instance.NewLayout(t.TempDir())
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: "paused-run", Workflow: "implementation", WorkflowVersion: 1,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("approval")
	if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: "approval"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	released := false
	if err := sweepStalledRuns(context.Background(), layout, nil, nil, nil, nil, nil, func(string, string) { released = true }, now, 45*time.Minute, 0); err != nil {
		t.Fatal(err)
	}

	assertWatchdogPhase(t, layout.RunsDir(), "paused-run", journal.PhaseRunning)
	if released {
		t.Fatal("paused human gate was released by stalled-run sweep")
	}
}

// TestSweepStalledRunsPreservesPausedGateBehindAPodPlaneEmit is the daemon-side
// half of the same protection. A mode-3 stage emits into the run's own journal
// through the write API's journal plane (livejournal.Writer.Adopt appends on
// the runner's handle), so an event can land AFTER the runner's gate.paused.
// While this sweep tested only the LAST event, such a run read "not parked" and
// a gate held past the timeout was escalated and its claim released — work a
// human was still deciding on, destroyed by the watchdog.
func TestSweepStalledRunsPreservesPausedGateBehindAPodPlaneEmit(t *testing.T) {
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	eventTime := now.Add(-2 * time.Hour)
	layout := instance.NewLayout(t.TempDir())
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: "paused-pod-run", Workflow: "implementation", WorkflowVersion: 1,
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil, journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("approval")
	if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: "approval"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordArtifactAnnotated("pr.json", []byte(`{"number":42}`),
		apiv1.IntegrityDerived, map[string]any{"emitKey": "paused-pod-run|0|open-pr|1|0"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	released := false
	if err := sweepStalledRuns(
		context.Background(),
		layout,
		nil,
		nil,
		nil,
		nil,
		nil,
		func(string, string) { released = true },
		now,
		45*time.Minute,
		0,
	); err != nil {
		t.Fatal(err)
	}

	assertWatchdogPhase(t, layout.RunsDir(), "paused-pod-run", journal.PhaseRunning)
	if released {
		t.Fatal("a gate still awaiting a human was escalated because a pod emit landed after gate.paused")
	}
}

func createWatchdogRunWithControls(t *testing.T, runsDir, runID, workflow, gaggle string, eventTime *time.Time, heartbeat time.Time, controls *apiv1.RunControls) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: workflow, WorkflowVersion: 1, Gaggle: gaggle,
		RunControls: controls,
		Trigger:     journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return *eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("implement")
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if !heartbeat.IsZero() {
		*eventTime = heartbeat
		if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "implement", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertWatchdogPhase(t *testing.T, runsDir, runID string, want journal.RunPhase) {
	t.Helper()
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	phase, err := reader.Phase()
	if err != nil {
		t.Fatal(err)
	}
	if phase != want {
		t.Fatalf("run %s phase = %s, want %s", runID, phase, want)
	}
}
