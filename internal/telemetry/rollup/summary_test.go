package rollup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

type summaryMutation struct {
	kind      string
	operation string
}

func seedSummaryRun(
	t *testing.T,
	runsDir, runID, workflow, status string,
	startedAt time.Time,
	agenticDuration time.Duration,
	mutations ...summaryMutation,
) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: "+workflow))

	seq := 1
	lines := []string{eventLine(seq, startedAt, `"type":"run.started"`)}
	seq++
	offset := time.Second
	if agenticDuration > 0 {
		lines = append(lines,
			eventLine(seq, startedAt.Add(offset), `"type":"stage.started","stage":"agent","attempt":1`),
			eventLine(seq+1, startedAt.Add(offset+time.Millisecond), `"type":"span.recorded","stage":"agent","name":"copilot.transcript","ref":{"digest":"sha256:abc","size":1}`),
			eventLine(seq+2, startedAt.Add(offset+agenticDuration), `"type":"stage.finished","stage":"agent","attempt":1,"status":"success"`),
		)
		seq += 3
		offset += agenticDuration + time.Second
	}
	for i, mutation := range mutations {
		payload := fmt.Sprintf(
			`"type":"ref.touched","externalRef":{"provider":"github","kind":%q,"id":%q},"runner":{"operation":%q}`,
			mutation.kind,
			fmt.Sprintf("%d", i+1),
			mutation.operation,
		)
		lines = append(lines, eventLine(seq, startedAt.Add(offset), payload))
		seq++
		offset += time.Second
	}
	lines = append(lines, eventLine(seq, startedAt.Add(offset), `"type":"run.finished","status":"`+status+`"`))
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
}

func writeInitCompletedLog(t *testing.T, schedulerDir string, at time.Time) {
	t.Helper()
	mustMkdirAll(t, schedulerDir)
	mustWriteFile(
		t,
		filepath.Join(schedulerDir, fileEvents),
		eventLine(1, at, `"type":"init.completed"`)+"\n",
	)
}

func TestInstanceSummaryStatsReconcilesLifetimeAndWindow(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)

	seedSummaryRun(t, runsDir, "1111111111111111eeeeeeeeeeeeeeee", "implement", "completed", now.Add(-48*time.Hour), 2*time.Second,
		summaryMutation{kind: "pr", operation: "open"},
		summaryMutation{kind: "issue", operation: "claim"},
	)
	seedSummaryRun(t, runsDir, "2222222222222222eeeeeeeeeeeeeeee", "implement", "failed", now.Add(-time.Hour), 5*time.Second,
		summaryMutation{kind: "pr", operation: "merge"},
		summaryMutation{kind: "issue", operation: "close"},
	)
	seedSummaryRun(t, runsDir, "3333333333333333eeeeeeeeeeeeeeee", "nominate", "aborted", now.Add(-30*time.Minute), 0,
		summaryMutation{kind: "issue", operation: "claim"},
	)

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	initCompletedAt := now.Add(-48 * time.Hour)
	schedulerDir := filepath.Join(tmp, "scheduler")
	writeInitCompletedLog(t, schedulerDir, initCompletedAt)
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}

	all, err := db.InstanceSummaryStats(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("InstanceSummaryStats: %v", err)
	}
	if all.TotalRuns != 3 || all.CompletedRuns != 1 || all.FailedRuns != 1 || all.AbortedRuns != 1 || all.SuccessRate != 0.5 {
		t.Fatalf("run summary = %#v", all)
	}
	if all.PullRequestsOpened != 1 || all.PullRequestsMerged != 1 || all.IssuesClaimed != 2 || all.IssuesClosed != 1 {
		t.Fatalf("mutation summary = %#v", all)
	}
	if all.BusiestWorkflow != "implement" || all.BusiestWorkflowRuns != 2 {
		t.Fatalf("busiest workflow = %q/%d", all.BusiestWorkflow, all.BusiestWorkflowRuns)
	}
	if all.AgenticStageAttempts != 2 || all.AvgAgenticStageDurationMs != 3500 || all.LongestAgenticStageMs != 5000 {
		t.Fatalf("agentic stage summary = %#v", all)
	}
	if all.LongestAgenticWorkflow != "implement" || all.LongestAgenticStage != "agent" || all.LongestAgenticRunID != "2222222222222222eeeeeeeeeeeeeeee" {
		t.Fatalf("longest agentic stage identity = %#v", all)
	}
	timeToFirstPR, err := db.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatalf("TimeToFirstPR: %v", err)
	}
	wantFirstPROpenAt := initCompletedAt.Add(4 * time.Second)
	if timeToFirstPR.Anchor != telemetry.TimeToFirstPRAnchor ||
		timeToFirstPR.InitCompletedAt == nil || !timeToFirstPR.InitCompletedAt.Equal(initCompletedAt) ||
		timeToFirstPR.FirstPROpenAt == nil || !timeToFirstPR.FirstPROpenAt.Equal(wantFirstPROpenAt) ||
		timeToFirstPR.Milliseconds == nil || *timeToFirstPR.Milliseconds != 4000 {
		t.Fatalf("time to first PR = %#v", timeToFirstPR)
	}

	windowed, err := db.InstanceSummaryStats(context.Background(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("InstanceSummaryStats windowed: %v", err)
	}
	if windowed.TotalRuns != 2 || windowed.CompletedRuns != 0 || windowed.FailedRuns != 1 || windowed.AbortedRuns != 1 {
		t.Fatalf("windowed run summary = %#v", windowed)
	}
	if windowed.PullRequestsOpened != 0 || windowed.PullRequestsMerged != 1 || windowed.IssuesClaimed != 1 || windowed.IssuesClosed != 1 {
		t.Fatalf("windowed mutation summary = %#v", windowed)
	}
	if windowed.AgenticStageAttempts != 1 || windowed.AvgAgenticStageDurationMs != 5000 || windowed.LongestAgenticStageMs != 5000 {
		t.Fatalf("windowed agentic stage summary = %#v", windowed)
	}
}

func TestInstanceSummaryStatsEmpty(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	got, err := db.InstanceSummaryStats(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("InstanceSummaryStats: %v", err)
	}
	if got != (InstanceSummary{}) {
		t.Fatalf("empty summary = %#v", got)
	}
	timeToFirstPR, err := db.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatalf("TimeToFirstPR: %v", err)
	}
	if timeToFirstPR.Anchor != telemetry.TimeToFirstPRAnchor ||
		timeToFirstPR.InitCompletedAt != nil ||
		timeToFirstPR.FirstPROpenAt != nil ||
		timeToFirstPR.Milliseconds != nil {
		t.Fatalf("empty time to first PR = %#v", timeToFirstPR)
	}
}

func TestTimeToFirstPRSurvivesRunDeletionAndRebuild(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	initCompletedAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	seedSummaryRun(t, runsDir, fixtureRunID, "implement", "completed", initCompletedAt.Add(time.Minute), 0)
	seedSummaryRun(
		t,
		runsDir,
		fixtureRunID2,
		"implement",
		"completed",
		initCompletedAt.Add(time.Hour),
		0,
		summaryMutation{kind: "pr", operation: "open"},
	)

	dbPath := filepath.Join(tmp, "telemetry.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seedAndIngest(t, db, runsDir)
	schedulerDir := filepath.Join(tmp, "scheduler")
	writeInitCompletedLog(t, schedulerDir, initCompletedAt)
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}
	for _, runID := range []string{fixtureRunID, fixtureRunID2} {
		if err := db.DeleteRun(context.Background(), runID); err != nil {
			t.Fatalf("DeleteRun(%s): %v", runID, err)
		}
	}
	wantPROpenAt := initCompletedAt.Add(time.Hour + time.Second)
	assertTimeToFirstPR(t, db, initCompletedAt, wantPROpenAt)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(runsDir); err != nil {
		t.Fatal(err)
	}
	if err := Rebuild(context.Background(), dbPath, runsDir, schedulerDir); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	rebuilt, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuilt.Close() }()
	assertTimeToFirstPR(t, rebuilt, initCompletedAt, wantPROpenAt)
}

func TestTimeToFirstPRRejectsPROpenBeforeInitCompletion(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	initCompletedAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	seedSummaryRun(
		t,
		runsDir,
		fixtureRunID,
		"implement",
		"completed",
		initCompletedAt.Add(-time.Hour),
		0,
		summaryMutation{kind: "pr", operation: "open"},
	)

	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), filepath.Join(runsDir, fixtureRunID)); err != nil {
		t.Fatalf("IngestRun(pre-init): %v", err)
	}
	metric, err := db.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt != nil || metric.FirstPROpenAt != nil || metric.Milliseconds != nil {
		t.Fatalf("TimeToFirstPR before init = %#v", metric)
	}
	schedulerDir := filepath.Join(tmp, "scheduler")
	writeInitCompletedLog(t, schedulerDir, initCompletedAt)
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}
	metric, err = db.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) ||
		metric.FirstPROpenAt != nil || metric.Milliseconds != nil {
		t.Fatalf("TimeToFirstPR with only pre-init PR = %#v", metric)
	}

	seedSummaryRun(
		t,
		runsDir,
		fixtureRunID2,
		"implement",
		"completed",
		initCompletedAt.Add(5*time.Minute),
		0,
		summaryMutation{kind: "pr", operation: "open"},
	)
	if err := db.IngestRun(context.Background(), filepath.Join(runsDir, fixtureRunID2)); err != nil {
		t.Fatalf("IngestRun(post-init): %v", err)
	}
	if err := db.IngestRun(context.Background(), filepath.Join(runsDir, fixtureRunID)); err != nil {
		t.Fatalf("re-IngestRun(pre-init): %v", err)
	}
	assertTimeToFirstPR(t, db, initCompletedAt, initCompletedAt.Add(5*time.Minute+time.Second))
}

func TestEarlierInitAnchorSelectsRetainedPROpen(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	earlierInit := time.Date(2026, time.July, 16, 10, 0, 0, 0, time.UTC)
	laterInit := earlierInit.Add(2 * time.Hour)
	firstPROpenAt := earlierInit.Add(time.Hour + time.Second)

	db := openTestDB(t, tmp)
	if err := db.recordTimeToFirstPR(context.Background(), laterInit, time.Time{}); err != nil {
		t.Fatalf("record later init: %v", err)
	}
	seedSummaryRun(
		t,
		runsDir,
		fixtureRunID,
		"implement",
		"completed",
		firstPROpenAt.Add(-time.Second),
		0,
		summaryMutation{kind: "pr", operation: "open"},
	)
	if err := db.IngestRun(context.Background(), filepath.Join(runsDir, fixtureRunID)); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	schedulerDir := filepath.Join(tmp, "scheduler")
	writeInitCompletedLog(t, schedulerDir, earlierInit)
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}
	assertTimeToFirstPR(t, db, earlierInit, firstPROpenAt)
}

func TestRebuildSelectsFirstPostInitPROpen(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	initCompletedAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	seedSummaryRun(
		t,
		runsDir,
		fixtureRunID,
		"implement",
		"completed",
		initCompletedAt.Add(-time.Hour),
		0,
		summaryMutation{kind: "pr", operation: "open"},
	)
	seedSummaryRun(
		t,
		runsDir,
		fixtureRunID2,
		"implement",
		"completed",
		initCompletedAt.Add(5*time.Minute),
		0,
		summaryMutation{kind: "pr", operation: "open"},
	)
	schedulerDir := filepath.Join(tmp, "scheduler")
	writeInitCompletedLog(t, schedulerDir, initCompletedAt)
	dbPath := filepath.Join(tmp, "telemetry.db")
	if err := Rebuild(context.Background(), dbPath, runsDir, schedulerDir); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	assertTimeToFirstPR(t, db, initCompletedAt, initCompletedAt.Add(5*time.Minute+time.Second))
}

func TestRebuildRecoversTimeToFirstPRFromJournalsWhenDatabaseIsUnreadable(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	initCompletedAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	seedSummaryRun(
		t,
		runsDir,
		fixtureRunID,
		"implement",
		"completed",
		initCompletedAt.Add(time.Minute),
		0,
		summaryMutation{kind: "pr", operation: "open"},
	)

	dbPath := filepath.Join(tmp, "telemetry.db")
	if err := os.WriteFile(dbPath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	schedulerDir := filepath.Join(tmp, "scheduler")
	writeInitCompletedLog(t, schedulerDir, initCompletedAt)
	if err := Rebuild(context.Background(), dbPath, runsDir, schedulerDir); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	rebuilt, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuilt.Close() }()
	assertTimeToFirstPR(t, rebuilt, initCompletedAt, initCompletedAt.Add(time.Minute+time.Second))
}

func TestFirstSuccessMilestoneMigrationBackfillsRetainedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	initCompletedAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	firstPROpenAt := initCompletedAt.Add(7 * time.Minute)
	if _, err := db.sql.Exec(`
		INSERT INTO runs (run_id, workflow, workflow_version, gaggle, started_at)
		VALUES ('legacy-run', 'implement', 1, 'web', ?)`,
		formatTime(initCompletedAt.Add(time.Minute)),
	); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}
	if _, err := db.sql.Exec(`
		INSERT INTO scheduler_events (seq, type, occurred_at)
		VALUES (1, 'init.completed', ?)`,
		formatTime(initCompletedAt),
	); err != nil {
		t.Fatalf("insert init completion: %v", err)
	}
	if _, err := db.sql.Exec(`
		INSERT INTO provider_mutations (
			run_id, seq, provider, kind, external_id, operation, occurred_at
		) VALUES
			('legacy-run', 1, 'github', 'pr', '41', 'open', ?),
			('legacy-run', 2, 'github', 'pr', '42', 'open', ?)`,
		formatTime(initCompletedAt.Add(-time.Minute)),
		formatTime(firstPROpenAt),
	); err != nil {
		t.Fatalf("insert legacy provider mutation: %v", err)
	}
	if _, err := db.sql.Exec(`DROP TABLE first_success_milestones`); err != nil {
		t.Fatalf("drop first-success milestone table: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE schema_meta SET version = 13`); err != nil {
		t.Fatalf("restore v13 schema version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade Open: %v", err)
	}
	defer func() { _ = upgraded.Close() }()
	assertTimeToFirstPR(t, upgraded, initCompletedAt, firstPROpenAt)
}

func TestChronologyMigrationRepairsPreInitMilestone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	initCompletedAt := time.Date(2026, time.July, 16, 12, 0, 0, 500_000_000, time.UTC)
	firstPROpenAt := initCompletedAt.Add(250 * time.Millisecond)
	if _, err := db.sql.Exec(`
		INSERT INTO runs (run_id, workflow, workflow_version, gaggle, started_at)
		VALUES ('legacy-run', 'implement', 1, 'web', ?)`,
		formatTime(initCompletedAt.Add(time.Minute)),
	); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}
	if _, err := db.sql.Exec(`
		INSERT INTO provider_mutations (
			run_id, seq, provider, kind, external_id, operation, occurred_at
		) VALUES ('legacy-run', 1, 'github', 'pr', '42', 'open', ?)`,
		firstPROpenAt.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert valid provider mutation: %v", err)
	}
	if _, err := db.sql.Exec(`
		UPDATE first_success_milestones
		SET init_completed_at = ?, first_pr_open_at = ?
		WHERE id = 1`,
		initCompletedAt.Format(time.RFC3339Nano),
		initCompletedAt.Truncate(time.Second).Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed pre-init milestone: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE schema_meta SET version = 14`); err != nil {
		t.Fatalf("restore v14 schema version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade Open: %v", err)
	}
	defer func() { _ = upgraded.Close() }()
	assertTimeToFirstPR(t, upgraded, initCompletedAt, firstPROpenAt)
}

func assertTimeToFirstPR(t *testing.T, db *DB, initCompletedAt, firstPROpenAt time.Time) {
	t.Helper()
	metric, err := db.TimeToFirstPR(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metric.InitCompletedAt == nil || !metric.InitCompletedAt.Equal(initCompletedAt) ||
		metric.FirstPROpenAt == nil || !metric.FirstPROpenAt.Equal(firstPROpenAt) {
		t.Fatalf("TimeToFirstPR = %#v, want %s to %s", metric, initCompletedAt, firstPROpenAt)
	}
	wantMilliseconds := firstPROpenAt.Sub(initCompletedAt).Milliseconds()
	if metric.Milliseconds == nil || *metric.Milliseconds != wantMilliseconds {
		t.Fatalf("TimeToFirstPR milliseconds = %v, want %d", metric.Milliseconds, wantMilliseconds)
	}
}
