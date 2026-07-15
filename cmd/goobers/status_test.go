package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

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
