package rollup

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	all, err := db.InstanceSummaryStats(time.Time{})
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

	windowed, err := db.InstanceSummaryStats(now.Add(-24 * time.Hour))
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
	got, err := db.InstanceSummaryStats(time.Time{})
	if err != nil {
		t.Fatalf("InstanceSummaryStats: %v", err)
	}
	if got != (InstanceSummary{}) {
		t.Fatalf("empty summary = %#v", got)
	}
}
