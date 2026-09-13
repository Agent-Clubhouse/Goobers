package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

func TestStartupRecoveryRunDirsIgnoresRetainedTerminalHistory(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4000; i++ {
		if err := os.Mkdir(filepath.Join(layout.RunsDir(), fmt.Sprintf("terminal-%04d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	activeDir := filepath.Join(layout.RunsDir(), "active-run")
	if err := os.Mkdir(activeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, time.September, 12, 20, 0, 0, 0, time.UTC)
	if err := store.UpsertRun(context.Background(), readmodel.Projection{Run: readmodel.RunRow{
		RunID:        "active-run",
		Gaggle:       "example",
		Workflow:     "implementation",
		Phase:        journal.PhaseRunning,
		StartedAt:    started,
		LastActivity: started,
		LastSeq:      1,
	}}); err != nil {
		t.Fatal(err)
	}

	var examined int
	runDirs, err := startupRecoveryRunDirs(context.Background(), layout, store, func(count int, _ bool) {
		examined = count
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runDirs) != 1 || runDirs[0] != activeDir {
		t.Fatalf("recovery inventory = %v, want only %s", runDirs, activeDir)
	}
	if examined != 1 {
		t.Fatalf("reported examined=%d, want 1 non-terminal candidate rather than 4001 retained directories", examined)
	}
}

func TestStartupRecoveryRunDirsIncludesMarkedTerminalCleanup(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	runID := "0123456789abcdef0123456789abcdef"
	runDir := filepath.Join(layout.RunsDir(), runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.MarkRunActive(layout.RunsDir(), runID); err != nil {
		t.Fatal(err)
	}

	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	finished := time.Date(2026, time.September, 12, 20, 1, 0, 0, time.UTC)
	if err := store.UpsertRun(context.Background(), readmodel.Projection{Run: readmodel.RunRow{
		RunID: runID, Gaggle: "example", Workflow: "implementation",
		Phase: journal.PhaseCompleted, Terminal: true,
		StartedAt: finished.Add(-time.Minute), FinishedAt: &finished,
		LastActivity: finished, LastSeq: 2,
	}}); err != nil {
		t.Fatal(err)
	}

	runDirs, err := startupRecoveryRunDirs(context.Background(), layout, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(runDirs) != 1 || runDirs[0] != runDir {
		t.Fatalf("recovery inventory = %v, want marked terminal cleanup %s", runDirs, runDir)
	}
}

func TestMarkedRecoveryRunDirsMovesLegacyMarkerToScopedRuntime(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	runID := "0123456789abcdef0123456789abcdef"
	legacyRunDir := filepath.Join(layout.RunsDir(), runID)
	if err := os.MkdirAll(legacyRunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.MarkRunActive(layout.RunsDir(), runID); err != nil {
		t.Fatal(err)
	}

	scopedRunsDir := layout.ForGaggle("example").RunsDir()
	if err := os.MkdirAll(filepath.Dir(scopedRunsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(layout.RunsDir(), scopedRunsDir); err != nil {
		t.Fatal(err)
	}

	runDirs, err := markedRecoveryRunDirs(layout)
	if err != nil {
		t.Fatal(err)
	}
	scopedRunDir := filepath.Join(scopedRunsDir, runID)
	if len(runDirs) != 1 || runDirs[0] != scopedRunDir {
		t.Fatalf("marked recovery dirs = %v, want [%s]", runDirs, scopedRunDir)
	}
	if legacy, err := journal.ActiveRunDirs(layout.RunsDir()); err != nil || len(legacy) != 0 {
		t.Fatalf("legacy active markers = %v, err=%v; want migrated away", legacy, err)
	}
	if scoped, err := journal.ActiveRunDirs(scopedRunsDir); err != nil || len(scoped) != 1 || scoped[0] != scopedRunDir {
		t.Fatalf("scoped active markers = %v, err=%v; want [%s]", scoped, err, scopedRunDir)
	}
}
