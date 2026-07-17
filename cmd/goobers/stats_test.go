package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func writeStatsCommandRun(t *testing.T, root, runID, workflow string, startedAt time.Time, phase journal.RunPhase) {
	t.Helper()
	now := startedAt
	clock := func() time.Time {
		current := now
		now = now.Add(time.Second)
		return current
	}
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        workflow,
		WorkflowVersion: 1,
		Gaggle:          "example",
		StartedAt:       startedAt,
	}, nil, journal.WithClock(clock))
	if err != nil {
		t.Fatalf("create stats fixture run: %v", err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordSpan("implement", "copilot.transcript", []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		kind      string
		operation string
	}{
		{kind: "pr", operation: "open"},
		{kind: "issue", operation: "claim"},
	} {
		if err := run.Append(journal.Event{
			Type:        journal.EventRefTouched,
			ExternalRef: &journal.ExternalRef{Provider: "github", Kind: mutation.kind, ID: runID},
			Runner:      map[string]any{"operation": mutation.operation},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		t.Fatal(err)
	}
}

func TestStatsJSONAndSinceWindow(t *testing.T) {
	root := initDemo(t)
	writeStatsCommandRun(t, root, "old-run", "implement", time.Now().Add(-48*time.Hour), journal.PhaseCompleted)
	writeStatsCommandRun(t, root, "recent-run", "nominate", time.Now().Add(-time.Hour), journal.PhaseFailed)
	l := instance.NewLayout(root)
	if err := rollup.Rebuild(l.TelemetryDB(), l.RunsDir(), l.SchedulerDir()); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}

	code, stdout, stderr := runArgs(t, "stats", "--since", "24h", "--json", root)
	if code != 0 {
		t.Fatalf("stats --json: code = %d, stderr = %q", code, stderr)
	}
	var got statsJSONSummary
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode stats JSON: %v\n%s", err, stdout)
	}
	if got.Since == nil {
		t.Fatal("stats JSON omitted the active since window")
	}
	if got.Runs.Total != 1 || got.Runs.ByPhase.Failed != 1 || got.Runs.ByPhase.Completed != 0 || got.Runs.SuccessRate != 0 {
		t.Fatalf("windowed runs = %#v", got.Runs)
	}
	if got.PullRequests.Opened != 1 || got.Issues.Claimed != 1 {
		t.Fatalf("windowed mutations = PR %#v, issues %#v", got.PullRequests, got.Issues)
	}
	if got.BusiestWorkflow == nil || got.BusiestWorkflow.Name != "nominate" || got.BusiestWorkflow.Runs != 1 {
		t.Fatalf("busiest workflow = %#v", got.BusiestWorkflow)
	}
	if got.AgenticStageDuration == nil || got.AgenticStageDuration.Attempts != 1 || got.AgenticStageDuration.LongestStage != "implement" {
		t.Fatalf("agentic duration = %#v", got.AgenticStageDuration)
	}
}

func TestStatsHumanCard(t *testing.T) {
	root := initDemo(t)
	writeStatsCommandRun(t, root, "completed-run", "implement", time.Now().Add(-time.Hour), journal.PhaseCompleted)
	l := instance.NewLayout(root)
	if err := rollup.Rebuild(l.TelemetryDB(), l.RunsDir(), l.SchedulerDir()); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}

	code, stdout, stderr := runArgs(t, "stats", root)
	if code != 0 {
		t.Fatalf("stats: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"Goobers stats",
		"1 total (completed 1",
		"100.0%",
		"1 opened, 0 merged",
		"1 claimed, 0 closed",
		"implement (1 runs)",
		"1 attempts",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stats output missing %q:\n%s", want, stdout)
		}
	}
}

func TestStatsEmptyInstance(t *testing.T) {
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "stats", root)
	if code != 0 {
		t.Fatalf("stats: code = %d, stderr = %q", code, stderr)
	}
	if stdout != "no runs yet — try goobers run <workflow>\n" {
		t.Fatalf("stdout = %q", stdout)
	}

	code, stdout, stderr = runArgs(t, "stats", "--json", root)
	if code != 0 {
		t.Fatalf("stats --json: code = %d, stderr = %q", code, stderr)
	}
	var got statsJSONSummary
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode empty stats JSON: %v", err)
	}
	if got.Runs.Total != 0 || got.BusiestWorkflow != nil || got.AgenticStageDuration != nil {
		t.Fatalf("empty stats JSON = %#v", got)
	}

	code, stdout, stderr = runArgs(t, "stats", "--since", "24h", root)
	if code != 0 {
		t.Fatalf("stats --since on empty instance: code = %d, stderr = %q", code, stderr)
	}
	if stdout != "no runs yet — try goobers run <workflow>\n" {
		t.Fatalf("windowed empty stdout = %q", stdout)
	}
}

func TestStatsSinceWithNoMatchingRuns(t *testing.T) {
	root := initDemo(t)
	writeStatsCommandRun(t, root, "old-run", "implement", time.Now().Add(-48*time.Hour), journal.PhaseCompleted)
	l := instance.NewLayout(root)
	if err := rollup.Rebuild(l.TelemetryDB(), l.RunsDir(), l.SchedulerDir()); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}

	code, stdout, stderr := runArgs(t, "stats", "--since", "24h", root)
	if code != 0 {
		t.Fatalf("stats --since: code = %d, stderr = %q", code, stderr)
	}
	if stdout != "no runs in the last 24h0m0s\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestStatsRejectsInvalidArguments(t *testing.T) {
	root := initDemo(t)
	for _, args := range [][]string{
		{"stats", "--since", "0s", root},
		{"stats", "--since", "-1h", root},
		{"stats", root, "extra"},
	} {
		code, _, stderr := runArgs(t, args...)
		if code != 2 {
			t.Fatalf("%v: code = %d, want 2", args, code)
		}
		if stderr == "" {
			t.Fatalf("%v: expected usage error", args)
		}
	}
}
