package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func writeStatusRun(t *testing.T, root, runID, workflow, gaggle string, startedAt time.Time) {
	writeStatusRunWithPhase(t, root, runID, workflow, gaggle, startedAt, journal.PhaseRunning)
}

func writeStatusRunWithPhase(
	t *testing.T,
	root, runID, workflow, gaggle string,
	startedAt time.Time,
	phase journal.RunPhase,
) {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID:     runID,
		Workflow:  workflow,
		Gaggle:    gaggle,
		StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatalf("create status fixture run: %v", err)
	}
	if phase != journal.PhaseRunning {
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
			t.Fatalf("finish status fixture run: %v", err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close status fixture run: %v", err)
	}
}

// TestStatusRejectsNonInstanceRoot is issue #142: a typo'd or otherwise
// nonexistent path used to fall through to listRuns finding no runs/ dir,
// printing the misleading "no runs found" at exit 0 — indistinguishable from
// a real, empty instance. It must now fail closed with an actionable error.
func TestStatusRejectsNonInstanceRoot(t *testing.T) {
	notAnInstance := filepath.Join(t.TempDir(), "typo-path")
	code, stdout, stderr := runArgs(t, "status", notAnInstance)
	if code == 0 {
		t.Fatalf("expected a non-zero exit for a non-instance-root path, got 0 (stdout=%q)", stdout)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("expected a not-an-instance-root error, got stderr = %q", stderr)
	}
}

// TestStatusOnRealInstanceWithNoRunsSucceeds proves the fix didn't regress
// the legitimate case: a real instance root that simply has no runs yet
// still reports actionable no-runs guidance at exit 0.
func TestStatusOnRealInstanceWithNoRunsSucceeds(t *testing.T) {
	root := initScheduledDemo(t)
	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	const want = "Issues parked on learned dependencies: 0\n" +
		"no runs found — trigger one with 'goobers run <workflow>'\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestStatusJSON(t *testing.T) {
	root := initScheduledDemo(t)
	oldStartedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	newStartedAt := oldStartedAt.Add(time.Hour)
	writeStatusRun(t, root, "a-new", "new-workflow", "new-gaggle", newStartedAt)
	writeStatusRun(t, root, "z-old", "old-workflow", "old-gaggle", oldStartedAt)

	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json: code = %d, stderr = %q", code, stderr)
	}
	var got statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("status JSON = %q: %v", stdout, err)
	}
	if len(got.Runs) != 2 ||
		got.Runs[0].RunID != "a-new" ||
		got.Runs[1].RunID != "z-old" ||
		!got.Runs[0].StartedAt.Equal(newStartedAt) ||
		!got.Runs[1].StartedAt.Equal(oldStartedAt) {
		t.Fatalf("runs = %+v", got.Runs)
	}
	for _, run := range got.Runs {
		if run.LastActivityAt.IsZero() {
			t.Fatalf("run %q has no last activity timestamp", run.RunID)
		}
	}
}

func TestListRunsSkipsNonRunEntry(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	newStuckRun(t, layout, "valid-run", "default-implement")
	if err := os.WriteFile(filepath.Join(layout.RunsDir(), "notes.txt"), []byte("not a run"), 0o644); err != nil {
		t.Fatal(err)
	}

	runs, err := listRuns(layout.RunsDir())
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("listRuns returned %d runs, want 1: %+v", len(runs), runs)
	}
	if got := runs[0]; got.RunID != "valid-run" || got.Workflow != "default-implement" || got.Gaggle != "example" || got.Phase != "running" {
		t.Fatalf("listRuns returned %+v, want the valid run summary", got)
	}
}

type stubStatusReadService struct {
	runs  []readservice.RunSummary
	err   error
	calls int
}

func (s *stubStatusReadService) ListStatusRuns(context.Context) ([]readservice.RunSummary, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.runs, nil
}

func (s *stubStatusReadService) SchedulerStatus(context.Context) (readservice.SchedulerStatus, error) {
	return readservice.SchedulerStatus{}, nil
}

func TestListStatusRunsUsesSingleSharedServiceProjection(t *testing.T) {
	startedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	reads := &stubStatusReadService{runs: []readservice.RunSummary{
		{
			ID: "newer", Workflow: "implementation", Gaggle: "goobers",
			Phase: journal.PhaseRunning, StartedAt: startedAt.Add(time.Minute),
		},
		{
			ID: "older", Workflow: "implementation", Gaggle: "goobers",
			Phase: journal.PhaseCompleted, StartedAt: startedAt,
		},
	}}

	runs, err := listStatusRuns(context.Background(), reads)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].RunID != "newer" || runs[1].RunID != "older" {
		t.Fatalf("listStatusRuns = %+v, want shared-service summaries", runs)
	}
	if reads.calls != 1 {
		t.Fatalf("ListStatusRuns calls = %d, want 1", reads.calls)
	}
}

func TestListStatusRunsPropagatesSharedServiceFailure(t *testing.T) {
	want := errors.New("read failure")
	_, err := listStatusRuns(context.Background(), &stubStatusReadService{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("listStatusRuns error = %v, want %v", err, want)
	}
}

func TestStatusSkipsMalformedHistoricalRun(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	startedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	writeStatusRun(t, root, "healthy-run", "implementation", "goobers", startedAt)
	writeStatusRun(t, root, "malformed-run", "implementation", "goobers", startedAt.Add(-time.Minute))
	if err := os.WriteFile(
		filepath.Join(layout.RunsDir(), "malformed-run", "run.yaml"),
		[]byte("not: [valid"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "healthy-run") || strings.Contains(stdout, "malformed-run") {
		t.Fatalf("stdout = %q, want only the healthy run", stdout)
	}
}

func TestStatusReadFailurePreservesUsageIOExitCode(t *testing.T) {
	root := initDemo(t)
	runsDir := instance.NewLayout(root).RunsDir()
	if err := os.RemoveAll(runsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "status", root)
	if code != 2 {
		t.Fatalf("status: code = %d, want 2; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "read runs directory") {
		t.Fatalf("stderr = %q, want shared-service read failure", stderr)
	}
}

func TestStatusJSONEmptyInstance(t *testing.T) {
	root := initScheduledDemo(t)
	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json: code = %d, stderr = %q", code, stderr)
	}
	if stdout != `{"warnings":[],"runs":[]}`+"\n" {
		t.Fatalf("stdout = %q, want an empty warnings/runs envelope", stdout)
	}
}

func TestStatusDefaultTableIncludesLastActivity(t *testing.T) {
	root := initScheduledDemo(t)
	startedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	writeStatusRun(t, root, "fixture-run", "fixture-workflow", "fixture", startedAt)

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"LAST ACTIVITY",
		"fixture-run",
		"fixture-workflow",
		startedAt.Format(time.RFC3339),
		" ago\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestStatusFiltersByPhase(t *testing.T) {
	root := initDemo(t)
	startedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	writeStatusRun(t, root, "running-run", "implementation", "goobers", startedAt)
	writeStatusRunWithPhase(
		t,
		root,
		"failed-run",
		"implementation",
		"goobers",
		startedAt.Add(time.Minute),
		journal.PhaseFailed,
	)

	code, stdout, stderr := runArgs(t, "status", "--phase=running", root)
	if code != 0 {
		t.Fatalf("status --phase=running: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "running-run") || strings.Contains(stdout, "failed-run") {
		t.Fatalf("stdout = %q, want only the running run", stdout)
	}
}

func TestStatusFiltersByWorkflowBeforeLimit(t *testing.T) {
	root := initDemo(t)
	startedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	writeStatusRun(t, root, "merge-old", "merge-review", "goobers", startedAt)
	writeStatusRunWithPhase(
		t,
		root,
		"merge-middle",
		"merge-review",
		"goobers",
		startedAt.Add(time.Minute),
		journal.PhaseFailed,
	)
	writeStatusRunWithPhase(
		t,
		root,
		"merge-new",
		"merge-review",
		"goobers",
		startedAt.Add(2*time.Minute),
		journal.PhaseEscalated,
	)
	writeStatusRun(t, root, "other-newest", "implementation", "goobers", startedAt.Add(3*time.Minute))

	code, stdout, stderr := runArgs(t, "status", "--workflow=merge-review", "--limit=2", root)
	if code != 0 {
		t.Fatalf("status --workflow --limit: code = %d, stderr = %q", code, stderr)
	}
	middleIndex := strings.Index(stdout, "merge-middle")
	newIndex := strings.Index(stdout, "merge-new")
	if middleIndex == -1 || newIndex == -1 || newIndex > middleIndex {
		t.Fatalf("stdout = %q, want the two newest merge-review runs newest first", stdout)
	}
	if strings.Contains(stdout, "merge-old") || strings.Contains(stdout, "other-newest") {
		t.Fatalf("stdout = %q, want workflow filter applied before the limit", stdout)
	}
}

func TestStatusJSONFiltersByMultiplePhases(t *testing.T) {
	root := initScheduledDemo(t)
	startedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	writeStatusRunWithPhase(t, root, "completed-run", "implementation", "goobers", startedAt, journal.PhaseCompleted)
	writeStatusRunWithPhase(
		t,
		root,
		"failed-run",
		"implementation",
		"goobers",
		startedAt.Add(time.Minute),
		journal.PhaseFailed,
	)
	writeStatusRunWithPhase(
		t,
		root,
		"escalated-run",
		"implementation",
		"goobers",
		startedAt.Add(2*time.Minute),
		journal.PhaseEscalated,
	)

	code, stdout, stderr := runArgs(t, "status", "--json", "--phase=failed,escalated", root)
	if code != 0 {
		t.Fatalf("status --json --phase: code = %d, stderr = %q", code, stderr)
	}
	var got statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("status JSON = %q: %v", stdout, err)
	}
	if len(got.Runs) != 2 ||
		got.Runs[0].RunID != "escalated-run" ||
		got.Runs[1].RunID != "failed-run" {
		t.Fatalf("runs = %+v", got.Runs)
	}
}

func TestStatusFiltersCompose(t *testing.T) {
	root := initScheduledDemo(t)
	startedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	writeStatusRunWithPhase(t, root, "merge-old", "merge-review", "goobers", startedAt, journal.PhaseFailed)
	writeStatusRunWithPhase(
		t,
		root,
		"merge-new",
		"merge-review",
		"goobers",
		startedAt.Add(time.Minute),
		journal.PhaseFailed,
	)
	writeStatusRun(t, root, "merge-running", "merge-review", "goobers", startedAt.Add(2*time.Minute))
	writeStatusRunWithPhase(
		t,
		root,
		"other-failed",
		"implementation",
		"goobers",
		startedAt.Add(3*time.Minute),
		journal.PhaseFailed,
	)

	code, stdout, stderr := runArgs(
		t,
		"status",
		"--json",
		"--phase=failed",
		"--workflow=merge-review",
		"--limit=1",
		root,
	)
	if code != 0 {
		t.Fatalf("status with composed filters: code = %d, stderr = %q", code, stderr)
	}
	var got statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("status JSON = %q: %v", stdout, err)
	}
	if len(got.Runs) != 1 ||
		got.Runs[0].RunID != "merge-new" ||
		!got.Runs[0].StartedAt.Equal(startedAt.Add(time.Minute)) {
		t.Fatalf("runs = %+v", got.Runs)
	}
}

func TestStatusRejectsInvalidFilters(t *testing.T) {
	tests := []struct {
		name       string
		arg        string
		wantStderr string
	}{
		{name: "unknown phase", arg: "--phase=waiting", wantStderr: `invalid phase "waiting"`},
		{name: "empty phase", arg: "--phase=running,", wantStderr: `invalid phase ""`},
		{name: "negative limit", arg: "--limit=-1", wantStderr: "--limit must be non-negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runArgs(t, "status", tt.arg)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, stderr)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestStatusWatchRejectsInvalidOutputModes(t *testing.T) {
	root := initDemo(t)
	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "json",
			args:       []string{"status", "--watch", "--json", root},
			wantStderr: "--watch cannot be used with --json",
		},
		{
			name:       "non-terminal",
			args:       []string{"status", "--watch", root},
			wantStderr: "--watch requires terminal stdout",
		},
		{
			name:       "zero interval",
			args:       []string{"status", "--watch", "--interval=0s", root},
			wantStderr: "--interval must be greater than zero",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runArgs(t, tt.args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, stderr)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestWatchStatusRepaintsFiltersAndHighlightsPhaseChangeForOneFrame(t *testing.T) {
	startedAt := time.Date(2026, time.July, 16, 12, 30, 0, 0, time.UTC)
	target := runSummary{
		RunID:     "target-run-with-a-long-id",
		Workflow:  "implementation-workflow",
		Gaggle:    "goobers",
		Phase:     journal.PhaseRunning,
		StartedAt: startedAt,
	}
	older := runSummary{
		RunID:     "older-run",
		Workflow:  target.Workflow,
		Gaggle:    "goobers",
		Phase:     journal.PhaseRunning,
		StartedAt: startedAt.Add(-time.Minute),
	}
	otherWorkflow := runSummary{
		RunID:     "other-workflow-run",
		Workflow:  "merge-review",
		Gaggle:    "goobers",
		Phase:     journal.PhaseRunning,
		StartedAt: startedAt.Add(time.Minute),
	}
	frames := [][]runSummary{
		{target, older, otherWorkflow},
		{withStatusPhase(target, journal.PhaseFailed), older, otherWorkflow},
		{withStatusPhase(target, journal.PhaseFailed), older, otherWorkflow},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frame := 0
	loadRuns := func() ([]runSummary, error) {
		runs := frames[frame]
		frame++
		if frame == len(frames) {
			cancel()
		}
		return runs, nil
	}
	options := statusOptions{
		phases: map[journal.RunPhase]struct{}{
			journal.PhaseRunning: {},
			journal.PhaseFailed:  {},
		},
		workflow: target.Workflow,
		limit:    1,
	}
	var stdout bytes.Buffer
	noStatusText := func() (string, error) { return "", nil }
	if err := watchStatus(ctx, time.Millisecond, options, &stdout, loadRuns, noStatusText); err != nil {
		t.Fatalf("watchStatus: %v", err)
	}

	output := stdout.String()
	if got := strings.Count(output, statusClearScreen); got != len(frames) {
		t.Fatalf("clear sequence count = %d, want %d (output=%q)", got, len(frames), output)
	}
	if strings.Contains(output, older.RunID) || strings.Contains(output, otherWorkflow.RunID) {
		t.Fatalf("output = %q, want workflow and limit filters honored", output)
	}
	failedRow := fmt.Sprintf(
		statusWatchRowFormat,
		target.RunID,
		target.Workflow,
		target.Gaggle,
		journal.PhaseFailed,
		"-",
	)
	if got := strings.Count(output, statusHighlight+failedRow+statusReset); got != 1 {
		t.Fatalf("highlighted failed row count = %d, want 1 (output=%q)", got, output)
	}

	plainOutput := strings.NewReplacer(
		statusClearScreen, "",
		statusHighlight, "",
		statusReset, "",
	).Replace(output)
	for _, line := range strings.Split(plainOutput, "\n") {
		if len(line) > 80 {
			t.Fatalf("watch line is %d columns, want at most 80: %q", len(line), line)
		}
	}
}

func TestFormatLastActivity(t *testing.T) {
	now := time.Date(2026, time.July, 20, 9, 1, 30, 0, time.UTC)
	if got := formatLastActivity(now, now.Add(-90*time.Second)); got != "1m30s ago" {
		t.Fatalf("formatLastActivity() = %q, want %q", got, "1m30s ago")
	}
	if got := formatLastActivity(now, now.Add(time.Second)); got != "0s ago" {
		t.Fatalf("future activity = %q, want %q", got, "0s ago")
	}
	if got := formatLastActivity(now, time.Time{}); got != "-" {
		t.Fatalf("missing activity = %q, want %q", got, "-")
	}
}

func TestStatusDaemonReportsLiveIdentityAndRunCount(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	startedAt := time.Now().UTC().Add(-90 * time.Minute)
	writeStatusRun(t, root, "live-run", "implementation", "goobers", startedAt)
	writeStatusRunWithPhase(
		t,
		root,
		"finished-run",
		"implementation",
		"goobers",
		startedAt.Add(time.Minute),
		journal.PhaseCompleted,
	)
	identity := daemonIdentity{
		PID:          os.Getpid(),
		StartedAt:    startedAt,
		InstanceRoot: root,
		Version:      "v0.3.0-test",
	}
	release, err := acquireInstanceLockWithIdentity(filepath.Join(l.SchedulerDir(), "up.lock"), &identity)
	if err != nil {
		t.Fatalf("acquire daemon fixture lock: %v", err)
	}
	defer release()

	code, stdout, stderr := runArgs(t, "status", "--daemon", root)
	if code != 0 {
		t.Fatalf("status --daemon: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		fmt.Sprintf("daemon running: pid %d", os.Getpid()),
		"uptime 1h30m",
		"version v0.3.0-test",
		"live runs 1",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// TestWatchStatusRepaintsProviderQuotaPauseLine confirms #712's pause line
// composes with #609's watch board: it's re-fetched (not cached) on every
// redraw — appearing once loadPauseLine starts returning non-empty, and
// gone the moment it goes back to empty (dispatch resumed) — right after
// the clear-screen escape, ahead of the run table.
func TestWatchStatusRepaintsProviderQuotaPauseLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loadRuns := func() ([]runSummary, error) { return nil, nil }

	frame := 0
	const pauseLine = "GitHub quota exhausted — resuming dispatch at 2026-07-17T05:00:00Z\n"
	loadStatusText := func() (string, error) {
		defer func() { frame++ }()
		switch frame {
		case 0:
			return pauseLine, nil
		case 1:
			cancel()
			return "", nil // resumed by the second frame
		default:
			return "", nil
		}
	}

	var stdout bytes.Buffer
	if err := watchStatus(ctx, time.Millisecond, statusOptions{}, &stdout, loadRuns, loadStatusText); err != nil {
		t.Fatalf("watchStatus: %v", err)
	}

	frames := strings.Split(stdout.String(), statusClearScreen)[1:] // drop the empty pre-first-clear split
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (output=%q)", len(frames), stdout.String())
	}
	if !strings.HasPrefix(frames[0], pauseLine) {
		t.Fatalf("frame 0 = %q, want it to start with the pause line", frames[0])
	}
	if strings.Contains(frames[1], "GitHub quota exhausted") {
		t.Fatalf("frame 1 = %q, want no pause line once dispatch resumed", frames[1])
	}
}

func withStatusPhase(run runSummary, phase journal.RunPhase) runSummary {
	run.Phase = phase
	return run
}

func TestStatusDaemonReportsStaleIdentity(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	startedAt := time.Date(2026, time.July, 16, 9, 0, 0, 0, time.UTC)
	identity := daemonIdentity{
		PID:          4242,
		StartedAt:    startedAt,
		InstanceRoot: root,
		Version:      "v0.3.0-test",
	}
	release, err := acquireInstanceLockWithIdentity(filepath.Join(l.SchedulerDir(), "up.lock"), &identity)
	if err != nil {
		t.Fatalf("acquire daemon fixture lock: %v", err)
	}
	release()

	code, stdout, stderr := runArgs(t, "status", "--daemon", root)
	if code != 1 {
		t.Fatalf("status --daemon: code = %d, want 1; stderr = %q", code, stderr)
	}
	want := "daemon not running (last daemon: pid 4242, started 2026-07-16T09:00:00Z); " +
		"version v0.3.0-test, live runs 0\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestStatusDaemonReportsNeverStarted(t *testing.T) {
	root := initDemo(t)

	code, stdout, stderr := runArgs(t, "status", "--daemon", root)
	if code != 1 {
		t.Fatalf("status --daemon: code = %d, want 1; stderr = %q", code, stderr)
	}
	if stdout != "daemon not running; live runs 0\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestStatusDaemonDoesNotMistakeManualLockForDaemon(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		t.Fatalf("acquire manual fixture lock: %v", err)
	}
	defer release()

	code, stdout, stderr := runArgs(t, "status", "--daemon", root)
	if code != 1 {
		t.Fatalf("status --daemon: code = %d, want 1; stderr = %q", code, stderr)
	}
	if stdout != "daemon not running; live runs 0\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestStatusDaemonDoesNotMistakeStaleIdentityForManualHolder(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	identity := daemonIdentity{
		PID:          os.Getpid(),
		StartedAt:    time.Date(2026, time.July, 16, 9, 0, 0, 0, time.UTC),
		InstanceRoot: root,
		Version:      "v0.3.0-test",
	}
	lockPath := filepath.Join(l.SchedulerDir(), "up.lock")
	release, err := acquireInstanceLockWithIdentity(lockPath, &identity)
	if err != nil {
		t.Fatalf("write stale daemon fixture: %v", err)
	}
	release()
	release, err = acquireInstanceLock(lockPath)
	if err != nil {
		t.Fatalf("acquire manual fixture lock: %v", err)
	}
	defer release()

	code, stdout, stderr := runArgs(t, "status", "--daemon", root)
	if code != 1 {
		t.Fatalf("status --daemon: code = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.HasPrefix(stdout, fmt.Sprintf("daemon not running (last daemon: pid %d, started ", os.Getpid())) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestStatusDaemonRejectsRunListingFlags(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "status", "--daemon", "--json", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--daemon cannot be combined") {
		t.Fatalf("stderr = %q", stderr)
	}
}
