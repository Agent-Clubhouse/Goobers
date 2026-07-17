package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
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
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	const want = "no runs found — trigger one with 'goobers run <workflow>'\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestStatusJSON(t *testing.T) {
	root := initDemo(t)
	oldStartedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	newStartedAt := oldStartedAt.Add(time.Hour)
	writeStatusRun(t, root, "a-new", "new-workflow", "new-gaggle", newStartedAt)
	writeStatusRun(t, root, "z-old", "old-workflow", "old-gaggle", oldStartedAt)

	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json: code = %d, stderr = %q", code, stderr)
	}
	want := fmt.Sprintf(
		`{"warnings":[],"runs":[{"runId":"z-old","workflow":"old-workflow","gaggle":"old-gaggle","phase":"running","startedAt":%q},{"runId":"a-new","workflow":"new-workflow","gaggle":"new-gaggle","phase":"running","startedAt":%q}]}`+"\n",
		oldStartedAt.Format(time.RFC3339), newStartedAt.Format(time.RFC3339),
	)
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
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

func TestStatusJSONEmptyInstance(t *testing.T) {
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json: code = %d, stderr = %q", code, stderr)
	}
	if stdout != `{"warnings":[],"runs":[]}`+"\n" {
		t.Fatalf("stdout = %q, want an empty warnings/runs envelope", stdout)
	}
}

func TestStatusDefaultTableOutputUnchanged(t *testing.T) {
	root := initDemo(t)
	startedAt := time.Date(2026, time.July, 14, 12, 30, 0, 0, time.UTC)
	writeStatusRun(t, root, "fixture-run", "fixture-workflow", "fixture", startedAt)

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	want := fmt.Sprintf(
		"%-34s  %-24s  %-10s  %-10s  %s\n%-34s  %-24s  %-10s  %-10s  %s\n",
		"RUN ID", "WORKFLOW", "GAGGLE", "PHASE", "STARTED",
		"fixture-run", "fixture-workflow", "fixture", "running", startedAt.Format(time.RFC3339),
	)
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
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
	if middleIndex == -1 || newIndex == -1 || middleIndex > newIndex {
		t.Fatalf("stdout = %q, want the two newest merge-review runs in ascending order", stdout)
	}
	if strings.Contains(stdout, "merge-old") || strings.Contains(stdout, "other-newest") {
		t.Fatalf("stdout = %q, want workflow filter applied before the limit", stdout)
	}
}

func TestStatusJSONFiltersByMultiplePhases(t *testing.T) {
	root := initDemo(t)
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
	want := fmt.Sprintf(
		`{"warnings":[],"runs":[{"runId":"failed-run","workflow":"implementation","gaggle":"goobers","phase":"failed","startedAt":%q},{"runId":"escalated-run","workflow":"implementation","gaggle":"goobers","phase":"escalated","startedAt":%q}]}`+"\n",
		startedAt.Add(time.Minute).Format(time.RFC3339),
		startedAt.Add(2*time.Minute).Format(time.RFC3339),
	)
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestStatusFiltersCompose(t *testing.T) {
	root := initDemo(t)
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
	want := fmt.Sprintf(
		`{"warnings":[],"runs":[{"runId":"merge-new","workflow":"merge-review","gaggle":"goobers","phase":"failed","startedAt":%q}]}`+"\n",
		startedAt.Add(time.Minute).Format(time.RFC3339),
	)
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
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
	noPauseLine := func() string { return "" }
	if err := watchStatus(ctx, time.Millisecond, options, &stdout, loadRuns, noPauseLine); err != nil {
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
		target.StartedAt.Format(time.RFC3339),
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
	loadPauseLine := func() string {
		defer func() { frame++ }()
		switch frame {
		case 0:
			return pauseLine
		case 1:
			cancel()
			return "" // resumed by the second frame
		default:
			return ""
		}
	}

	var stdout bytes.Buffer
	if err := watchStatus(ctx, time.Millisecond, statusOptions{}, &stdout, loadRuns, loadPauseLine); err != nil {
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
