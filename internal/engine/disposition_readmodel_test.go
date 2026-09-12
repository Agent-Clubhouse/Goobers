package engine

import (
	"database/sql"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/readmodel"
)

// TestDSL3TerminalRunsHaveClosedDispositionAccounting exercises the complete
// engine -> immutable journal -> SQLite read-model path. In particular, it
// guards the DSL 3 seam that #4882 exposed: a useful run must be counted as
// produced, an idle first-stage short circuit as no-work, and no terminal row
// may fall through to unknown.
func TestDSL3TerminalRunsHaveClosedDispositionAccounting(t *testing.T) {
	runsDir := filepath.Join(t.TempDir(), "runs")
	spec := crSpec("poll", []apiv1.Task{crTask("poll", "")}, nil)

	for _, tc := range []struct {
		name   string
		stages *scriptedStages
	}{
		{
			name: "no-work",
			stages: &scriptedStages{results: map[string][]apiv1.ResultEnvelope{
				"poll": {{Status: apiv1.ResultNoWork, Summary: "queue empty"}},
			}},
		},
		{name: "produced", stages: &scriptedStages{}},
	} {
		in := projectionInput("dsl3-disposition-"+tc.name, spec)
		in.DSLVersion = "3.0"
		projection := executeForProjection(t, in, &Activities{
			Det:        tc.stages,
			Workspaces: testWorkspaces(t),
		}, false)
		if _, err := ProjectRun(runsDir, projection); err != nil {
			t.Fatalf("project %s engine journal: %v", tc.name, err)
		}
	}

	dbPath := filepath.Join(t.TempDir(), readmodel.FileName)
	store, err := readmodel.Open(dbPath)
	if err != nil {
		t.Fatalf("open read model: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if result, err := store.BuildFromJournals(t.Context(), []string{runsDir}); err != nil {
		t.Fatalf("build read model: %v", err)
	} else if result.Projected != 2 {
		t.Fatalf("projected runs = %d, want 2", result.Projected)
	}

	// Query SQLite directly: this is the product accounting invariant, rather
	// than an assertion over an in-memory projector value that could disagree
	// with what UpsertRun persists.
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open SQL verifier: %v", err)
	}
	defer db.Close()
	for disposition, want := range map[string]int{
		readmodel.DispositionUnknown:  0,
		readmodel.DispositionProduced: 1,
		readmodel.DispositionNoWork:   1,
	} {
		var got int
		if err := db.QueryRowContext(t.Context(),
			`SELECT count(*) FROM run WHERE terminal = 1 AND disposition = ?`, disposition,
		).Scan(&got); err != nil {
			t.Fatalf("count terminal %s rows: %v", disposition, err)
		}
		if got != want {
			t.Errorf("terminal %s rows = %d, want %d", disposition, got, want)
		}
	}
}
