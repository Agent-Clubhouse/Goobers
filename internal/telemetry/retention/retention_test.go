package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
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
		{id: "active.telemetry-pruning", age: 55 * 24 * time.Hour, state: "active"},
		{id: "active-old", age: 60 * 24 * time.Hour, state: "active"},
		{id: "paused.telemetry-pruning", age: 65 * 24 * time.Hour, state: "paused"},
	}
	for _, fixture := range fixtures {
		dir := createRetentionRun(t, runLayout, fixture.id, now.Add(-fixture.age), fixture.state)
		if err := db.IngestRun(context.Background(), dir); err != nil {
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
	for _, runID := range []string{
		"keep-newest", "keep-second", "keep-third", "paused-old", "active-old",
		"active.telemetry-pruning", "paused.telemetry-pruning",
	} {
		if _, err := os.Stat(filepath.Join(runLayout.RunsDir(), runID)); err != nil {
			t.Fatalf("retained journal %s missing: %v", runID, err)
		}
	}
	assertRollupRunIDs(t, db,
		"active-old", "active.telemetry-pruning", "keep-newest", "keep-second",
		"keep-third", "paused-old", "paused.telemetry-pruning",
	)

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	runRoots, err := layout.RunDirs()
	if err != nil {
		t.Fatal(err)
	}
	if err := rollup.RebuildAll(context.Background(), layout.TelemetryDB(), runRoots, layout.SchedulerDir()); err != nil {
		t.Fatalf("RebuildAll: %v", err)
	}
	rebuilt, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuilt.Close() }()
	assertRollupRunIDs(t, rebuilt,
		"active-old", "active.telemetry-pruning", "keep-newest", "keep-second",
		"keep-third", "paused-old", "paused.telemetry-pruning",
	)
}

func TestPrunePreservesTimeToFirstPRMilestone(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	initCompletedAt := now.Add(-48 * time.Hour)
	instanceLog, _, err := journal.OpenInstanceLog(
		layout.SchedulerDir(),
		journal.WithClock(func() time.Time { return initCompletedAt }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Append(journal.Event{Type: journal.EventInitCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Close(); err != nil {
		t.Fatal(err)
	}
	firstDir := createRetentionRun(t, layout, "first-run", initCompletedAt.Add(time.Minute), "terminal")
	firstPROpenAt := initCompletedAt.Add(time.Hour + 10*time.Minute)
	prDir := createRetentionRun(
		t,
		layout,
		"first-pr-run",
		initCompletedAt.Add(time.Hour),
		"terminal",
		10*time.Minute,
	)
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestSchedulerLog(context.Background(), layout.SchedulerDir()); err != nil {
		t.Fatalf("ingest scheduler log: %v", err)
	}
	for _, dir := range []string{firstDir, prDir} {
		if err := db.IngestRun(context.Background(), dir); err != nil {
			t.Fatalf("ingest %s: %v", dir, err)
		}
	}

	results, err := Prune(
		layout,
		db,
		Policy{Window: 24 * time.Hour, MaxRuns: 500},
		Options{Now: now},
	)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Prune removed %d runs, want 2", len(results))
	}
	metric, err := db.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) ||
		metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(firstPROpenAt) {
		t.Fatalf("TimeToFirstPR after prune = %#v", metric)
	}
}

func TestPruneRejectsSchemaOnlyFutureJournal(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layout := instance.NewLayout(root)
	stagedRun := createRetentionRun(t, layout, "staged-run", now.Add(-48*time.Hour), "terminal")
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), stagedRun); err != nil {
		t.Fatal(err)
	}
	staged := stagedRunDir(stagedRun)
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stagedRun, staged); err != nil {
		t.Fatal(err)
	}

	futureDir := filepath.Join(layout.RunsDir(), "future")
	if err := os.MkdirAll(futureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	schema, err := json.Marshal(journal.SchemaInfo{
		Version:       journal.CurrentSchemaVersion + 1,
		MinimumBinary: "v2.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(futureDir, "schema.json"), schema, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Prune(
		layout,
		db,
		Policy{Window: 24 * time.Hour, MaxRuns: 500},
		Options{Now: now},
	)
	if err == nil {
		t.Fatal("Prune skipped a schema-only future journal")
	}
	for _, want := range []string{"version 2", "supported version 1", "minimum binary is v2.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Prune error %q does not contain %q", err, want)
		}
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("Prune removed staged journal before rejecting future schema: %v", err)
	}
	assertRollupRunIDs(t, db, "staged-run")
}

func TestPrunePreflightsAllStagedJournalSchemas(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layout := instance.NewLayout(root)
	runDir := createRetentionRun(t, layout, "a-staged", now.Add(-48*time.Hour), "terminal")
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}
	staged := stagedRunDir(runDir)
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(runDir, staged); err != nil {
		t.Fatal(err)
	}
	futureDir := filepath.Join(filepath.Dir(staged), "z-future")
	if err := os.MkdirAll(futureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	schema, err := json.Marshal(journal.SchemaInfo{
		Version:       journal.CurrentSchemaVersion + 1,
		MinimumBinary: "v2.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(futureDir, "schema.json"), schema, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Prune(
		layout,
		db,
		Policy{Window: 24 * time.Hour, MaxRuns: 500},
		Options{Now: now},
	)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("Prune error = %v, want unsupported staged schema", err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("Prune removed earlier staged journal during preflight: %v", err)
	}
	assertRollupRunIDs(t, db, "a-staged")
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
	if err := db.IngestRun(context.Background(), runDir); err != nil {
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
	if _, err := os.Stat(stagedRunDir(runDir)); !os.IsNotExist(err) {
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
	if err := db.IngestRun(context.Background(), runDir); err != nil {
		t.Fatal(err)
	}
	reserved, err := journal.ReserveTerminalForPrune(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reserved {
		t.Fatal("terminal run was not reserved")
	}
	staged := stagedRunDir(runDir)
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(runDir, staged); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(staged, ".telemetry-pruning")); err != nil {
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

func TestPruneSerializesWithInFlightIngestion(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := createRetentionRun(t, layout, "ingest-race", now.Add(-48*time.Hour), "terminal")
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	blocker, err := sql.Open("sqlite", layout.TelemetryDB()+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close() }()
	tx, err := blocker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE schema_meta SET version = version`); err != nil {
		t.Fatal(err)
	}

	ingestErr := make(chan error, 1)
	go func() {
		ingestErr <- db.IngestRun(context.Background(), runDir)
	}()
	waitForRunLock(t, runDir)

	results, err := Prune(layout, db, Policy{Window: 24 * time.Hour, MaxRuns: 500}, Options{Now: now})
	if err != nil {
		t.Fatalf("Prune while ingesting: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Prune while ingesting removed %v", results)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ingestErr:
		if err != nil {
			t.Fatalf("IngestRun: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IngestRun did not finish")
	}
	assertRollupRunIDs(t, db, "ingest-race")

	results, err = Prune(layout, db, Policy{Window: 24 * time.Hour, MaxRuns: 500}, Options{Now: now})
	if err != nil {
		t.Fatalf("Prune after ingesting: %v", err)
	}
	if len(results) != 1 || results[0].RunID != "ingest-race" {
		t.Fatalf("Prune after ingesting = %v, want ingest-race", results)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("pruned journal still exists: %v", err)
	}
	assertRollupRunIDs(t, db)
}

func TestIngestRunHonorsPruneReservation(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := createRetentionRun(t, layout, "reserved-run", now, "terminal")
	reserved, err := journal.ReserveTerminalForPrune(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reserved {
		t.Fatal("terminal run was not reserved")
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	err = db.IngestRun(context.Background(), runDir)
	if !errors.Is(err, journal.ErrPruneReserved) {
		t.Fatalf("IngestRun error = %v, want ErrPruneReserved", err)
	}
	assertRollupRunIDs(t, db)
}

func waitForRunLock(t *testing.T, runDir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, err := platformlock.TryAcquire(filepath.Join(runDir, ".lock"))
		if errors.Is(err, platformlock.ErrHeld) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.Release(); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("IngestRun did not acquire the journal lock")
		}
		time.Sleep(10 * time.Millisecond) // Polling interval for acquisition of the filesystem journal lock.
	}
}

func createRetentionRun(
	t *testing.T,
	layout instance.Layout,
	runID string,
	startedAt time.Time,
	state string,
	prOpenAfter ...time.Duration,
) string {
	t.Helper()
	if len(prOpenAfter) > 1 {
		t.Fatal("createRetentionRun accepts at most one PR-open offset")
	}
	now := startedAt
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordArtifact("transcript.jsonl", []byte("{\"role\":\"assistant\"}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan("implement", "harness.transcript", []byte("transcript")); err != nil {
		t.Fatal(err)
	}
	if len(prOpenAfter) == 1 {
		now = startedAt.Add(prOpenAfter[0])
		if err := run.Append(journal.Event{
			Type:        journal.EventRefTouched,
			ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42"},
			Runner:      map[string]any{"operation": "open"},
		}); err != nil {
			t.Fatal(err)
		}
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
	runs, err := db.Runs(context.Background())
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
