package main

import (
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
		`[{"runId":"z-old","workflow":"old-workflow","gaggle":"old-gaggle","phase":"running","startedAt":%q},{"runId":"a-new","workflow":"new-workflow","gaggle":"new-gaggle","phase":"running","startedAt":%q}]`+"\n",
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
	if stdout != "[]\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "[]\n")
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
		`[{"runId":"failed-run","workflow":"implementation","gaggle":"goobers","phase":"failed","startedAt":%q},{"runId":"escalated-run","workflow":"implementation","gaggle":"goobers","phase":"escalated","startedAt":%q}]`+"\n",
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
		`[{"runId":"merge-new","workflow":"merge-review","gaggle":"goobers","phase":"failed","startedAt":%q}]`+"\n",
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
