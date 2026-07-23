package retention

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestPruneAppliesBothBoundsProtectsLiveRunsAndRebuilds(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layout := instance.NewLayout(root)
	runLayout := layout.ForGaggle("example")
	if err := layout.EnsureGaggleRuntime("example"); err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}

	fixtures := []struct {
		id    string
		age   time.Duration
		state string
	}{
		{id: "keep-newest", age: 24 * time.Hour, state: "terminal"},
		{id: "keep-second", age: 2 * 24 * time.Hour, state: "terminal"},
		{id: "keep-third", age: 3 * 24 * time.Hour, state: "terminal"},
		{id: "max-candidate", age: 4 * 24 * time.Hour, state: "terminal"},
		{id: "window-candidate", age: 40 * 24 * time.Hour, state: "terminal"},
		{id: "paused-old", age: 50 * 24 * time.Hour, state: "paused"},
		{id: "active-old", age: 60 * 24 * time.Hour, state: "active"},
	}
	for _, fixture := range fixtures {
		dir := createRetentionRun(t, runLayout, fixture.id, now.Add(-fixture.age), fixture.state)
		if err := db.IngestRun(dir); err != nil {
			t.Fatalf("ingest %s: %v", fixture.id, err)
		}
	}

	results, err := Prune(layout, db, Policy{Window: 30 * 24 * time.Hour, MaxRuns: 3}, Options{Now: now})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got := make(map[string]string, len(results))
	for _, result := range results {
		got[result.RunID] = result.Reason
	}
	if got["max-candidate"] != "maxRuns" || got["window-candidate"] != "window,maxRuns" || len(got) != 2 {
		t.Fatalf("pruned = %#v, want max and window candidates", got)
	}
	for _, runID := range []string{"max-candidate", "window-candidate"} {
		if _, err := os.Stat(filepath.Join(runLayout.RunsDir(), runID)); !os.IsNotExist(err) {
			t.Fatalf("pruned journal %s still exists: %v", runID, err)
		}
	}
	for _, runID := range []string{"keep-newest", "keep-second", "keep-third", "paused-old", "active-old"} {
		if _, err := os.Stat(filepath.Join(runLayout.RunsDir(), runID)); err != nil {
			t.Fatalf("retained journal %s missing: %v", runID, err)
		}
	}
	assertRollupRunIDs(t, db, "active-old", "keep-newest", "keep-second", "keep-third", "paused-old")

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	runRoots, err := layout.RunDirs()
	if err != nil {
		t.Fatal(err)
	}
	if err := rollup.RebuildAll(layout.TelemetryDB(), runRoots, layout.SchedulerDir()); err != nil {
		t.Fatalf("RebuildAll: %v", err)
	}
	rebuilt, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuilt.Close() }()
	assertRollupRunIDs(t, rebuilt, "active-old", "keep-newest", "keep-second", "keep-third", "paused-old")
}

func TestPruneRestoresJournalWhenRollupDeletionFails(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := createRetentionRun(t, layout, "rollback-run", now.Add(-48*time.Hour), "terminal")
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.IngestRun(runDir); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Prune(layout, db, Policy{Window: 24 * time.Hour, MaxRuns: 500}, Options{Now: now}); err == nil {
		t.Fatal("Prune succeeded with a closed rollup")
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("journal was not restored: %v", err)
	}
	if _, err := os.Stat(runDir + stagingSuffix); !os.IsNotExist(err) {
		t.Fatalf("staged journal remains after rollback: %v", err)
	}
	reopened, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertRollupRunIDs(t, reopened, "rollback-run")
}

func TestPruneFinishesPartiallyRemovedStagedJournal(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := createRetentionRun(t, layout, "interrupted-run", now.Add(-48*time.Hour), "terminal")
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(runDir); err != nil {
		t.Fatal(err)
	}
	staged := runDir + stagingSuffix
	if err := os.Rename(runDir, staged); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(staged, "run.yaml")); err != nil {
		t.Fatal(err)
	}

	if _, err := Prune(layout, db, Policy{Window: 365 * 24 * time.Hour, MaxRuns: 500}, Options{Now: now}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("partially removed staged journal remains: %v", err)
	}
	assertRollupRunIDs(t, db)
}

func createRetentionRun(t *testing.T, layout instance.Layout, runID string, startedAt time.Time, state string) string {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return startedAt }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordArtifact("transcript.jsonl", []byte("{\"role\":\"assistant\"}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan("implement", "harness.transcript", []byte("transcript")); err != nil {
		t.Fatal(err)
	}
	switch state {
	case "terminal":
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
	case "paused":
		if err := run.Append(journal.Event{Type: journal.EventGatePaused, Gate: "approval"}); err != nil {
			t.Fatal(err)
		}
	case "active":
	default:
		t.Fatalf("unknown fixture state %q", state)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return run.Dir()
}

func assertRollupRunIDs(t *testing.T, db *rollup.DB, want ...string) {
	t.Helper()
	runs, err := db.Runs()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(runs))
	for _, run := range runs {
		got = append(got, run.RunID)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("rollup runs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rollup runs = %v, want %v", got, want)
		}
	}
}
