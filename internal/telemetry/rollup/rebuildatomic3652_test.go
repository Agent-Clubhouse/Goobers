package rollup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rebuiltRunIDs rebuilds nothing — it just reads back the run ids the rollup at
// dbPath currently projects, through a fresh handle, the way a separate
// `goobers telemetry` process would.
func rebuiltRunIDs(t *testing.T, dbPath string) []string {
	t.Helper()
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	runs, err := db.Runs(context.Background())
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.RunID)
	}
	return ids
}

func stagingLeftovers(t *testing.T, dir, dbName string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var leftovers []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+dbName+".rebuild-") {
			leftovers = append(leftovers, e.Name())
		}
	}
	return leftovers
}

// TestRebuildFailureKeepsPreviousProjection is issue #3652: a rebuild used to
// delete the active telemetry.db before decoding a single journal, so one
// malformed run destroyed the last usable projection and left a partial one in
// its place. The rebuild must now stage the whole projection first, so a
// failure mid-ingest leaves the previous database exactly as it was.
func TestRebuildFailureKeepsPreviousProjection(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dbPath := filepath.Join(tmp, "telemetry.db")
	schedulerDir := filepath.Join(tmp, "scheduler")
	ctx := context.Background()

	writeFixtureRun(t, runsDir, fixtureRunID, fixtureStart)
	if err := Rebuild(ctx, dbPath, runsDir, schedulerDir); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if got := rebuiltRunIDs(t, dbPath); len(got) != 1 || got[0] != fixtureRunID {
		t.Fatalf("run ids after first rebuild = %v, want [%s]", got, fixtureRunID)
	}

	corruptBody := rawEventLine(1, "run.started") + "\n" + `{not valid json` + "\n"
	writeRunWithRawEvents(t, runsDir, fixtureRunID2, corruptBody, "")

	err := Rebuild(ctx, dbPath, runsDir, schedulerDir)
	if err == nil {
		t.Fatal("Rebuild: want an error for the corrupt journal, got nil")
	}
	if !strings.Contains(err.Error(), fixtureRunID2) {
		t.Fatalf("Rebuild error = %v, want it to name the offending run directory %s", err, fixtureRunID2)
	}

	if got := rebuiltRunIDs(t, dbPath); len(got) != 1 || got[0] != fixtureRunID {
		t.Fatalf("run ids after failed rebuild = %v, want the previous projection [%s] intact", got, fixtureRunID)
	}
	if leftovers := stagingLeftovers(t, tmp, "telemetry.db"); len(leftovers) != 0 {
		t.Fatalf("staging files left behind after a failed rebuild: %v", leftovers)
	}
}

// TestRebuildReplacesRatherThanMergesWithPreviousProjection guards the other
// side of staging: the staged database must actually replace the active one, so
// rows for a run whose directory is gone do not survive a rebuild.
func TestRebuildReplacesRatherThanMergesWithPreviousProjection(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dbPath := filepath.Join(tmp, "telemetry.db")
	schedulerDir := filepath.Join(tmp, "scheduler")
	ctx := context.Background()

	writeFixtureRun(t, runsDir, fixtureRunID, fixtureStart)
	writeFixtureRun(t, runsDir, fixtureRunID2, fixtureStart)
	if err := Rebuild(ctx, dbPath, runsDir, schedulerDir); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if got := rebuiltRunIDs(t, dbPath); len(got) != 2 {
		t.Fatalf("run ids after first rebuild = %v, want both runs", got)
	}

	if err := os.RemoveAll(filepath.Join(runsDir, fixtureRunID2)); err != nil {
		t.Fatalf("remove run dir: %v", err)
	}
	if err := Rebuild(ctx, dbPath, runsDir, schedulerDir); err != nil {
		t.Fatalf("Rebuild after retention: %v", err)
	}

	got := rebuiltRunIDs(t, dbPath)
	if len(got) != 1 || got[0] != fixtureRunID {
		t.Fatalf("run ids after second rebuild = %v, want only [%s]", got, fixtureRunID)
	}
	if leftovers := stagingLeftovers(t, tmp, "telemetry.db"); len(leftovers) != 0 {
		t.Fatalf("staging files left behind after a successful rebuild: %v", leftovers)
	}
}
