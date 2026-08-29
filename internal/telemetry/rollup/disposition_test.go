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

// writeTerminalDispositionRun writes a run that fails at its implement stage
// with the given terminal cause: the run_failed error event carries
// terminalClass in the runner namespace exactly as internal/runner's
// failTerminal/finishStageFailure now write it (#3361), or no class at all
// when terminalClass is empty (a producer older than the refinement).
func writeTerminalDispositionRun(t *testing.T, runsDir, runID, stageCode, terminalClass string, startedAt time.Time) string {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	runYAML := fmt.Sprintf(`schema: goobers.dev/journal/run/v1
runId: %s
workflow: implementation
workflowVersion: 1
gaggle: web
trigger:
  kind: item
  ref: issue-2458
startedAt: %s
`, runID, startedAt.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte(runYAML), 0o644); err != nil {
		t.Fatalf("write run.yaml: %v", err)
	}
	ts := func(offset int) string {
		return startedAt.Add(time.Duration(offset) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	runnerJSON := ""
	if terminalClass != "" {
		runnerJSON = fmt.Sprintf(`,"runner":{"errorClass":%q}`, terminalClass)
	}
	lines := []string{
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":%q,"type":"run.started"}`, ts(0)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":%q,"type":"stage.started","stage":"implement","attempt":1,"attemptClass":"policy"}`, ts(1)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":3,"branch":0,"time":%q,"type":"stage.finished","stage":"implement","attempt":1,"status":"failure","error":{"code":%q,"message":"stage failed"}}`, ts(2), stageCode),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":4,"branch":0,"time":%q,"type":"error","stage":"implement","error":{"code":"run_failed","message":"%s: terminal"}%s}`, ts(3), stageCode, runnerJSON),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":5,"branch":0,"time":%q,"type":"run.finished","status":"failed"}`, ts(4)),
	}
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}
	return dir
}

// TestSuccessRateExcludesInfraFaultTerminals is #3364's regression against the
// live 2026-08-20 reading: goobers/implementation showed 0/10 0% on an
// instance whose implementation lane had been blocked by an infrastructure
// fault. An infra-fault terminal carries no verdict about the WORK, so it must
// be disclosed as InfraFailedRuns and dropped from the success-rate
// denominator instead of scoring the lane at zero.
//
// The corpus: one completed run, one infra-fault failure (credential
// materialization — #3361's exact shape), one genuine work failure. The
// honest rate is therefore 1 of 2 verdicts = 50%, not 1 of 3 = 33%.
func TestSuccessRateExcludesInfraFaultTerminals(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	db := openTestDB(t, tmp)
	ctx := context.Background()

	completed := writeMinimalFixtureRun(t, runsDir, fixtureRunID, fixtureStart)
	infra := writeTerminalDispositionRun(t, runsDir, fixtureRunID2,
		telemetry.ErrCodeCredentialUnavailable, string(telemetry.ErrorClassInfra), fixtureStart)
	work := writeTerminalDispositionRun(t, runsDir, "6bf92f3577b34da6a3ce929d0e0e4738",
		"nonzero_exit", string(telemetry.ErrorClassUnknown), fixtureStart)
	for _, dir := range []string{completed, infra, work} {
		if err := db.IngestRun(ctx, dir); err != nil {
			t.Fatalf("IngestRun %s: %v", dir, err)
		}
	}

	summary, err := db.InstanceSummaryStats(ctx, time.Time{})
	if err != nil {
		t.Fatalf("InstanceSummaryStats: %v", err)
	}
	if summary.FailedRuns != 2 {
		t.Fatalf("FailedRuns = %d, want 2 (both failures still counted honestly)", summary.FailedRuns)
	}
	if summary.InfraFailedRuns != 1 {
		t.Fatalf("InfraFailedRuns = %d, want 1 — the infra fault must be disclosed, not silently dropped", summary.InfraFailedRuns)
	}
	if summary.SuccessRate != 0.5 {
		t.Fatalf("SuccessRate = %v, want 0.5 (1 completed of 2 WORK verdicts; the infra fault is not a verdict)", summary.SuccessRate)
	}

	stats, err := db.Stats(ctx, StatsRequest{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	var gaggle *GaggleStats
	for i := range stats.Gaggles {
		if stats.Gaggles[i].Gaggle == "web" {
			gaggle = &stats.Gaggles[i]
		}
	}
	if gaggle == nil {
		t.Fatalf("no gaggle stats for web: %+v", stats.Gaggles)
	}
	if gaggle.InfraFailedRuns != 1 || gaggle.SuccessRate != 0.5 {
		t.Fatalf("gaggle stats = %+v, want infraFailedRuns 1 and successRate 0.5", *gaggle)
	}

	var implementation *RunStats
	for i := range stats.Runs {
		if stats.Runs[i].Workflow == "implementation" {
			implementation = &stats.Runs[i]
		}
	}
	if implementation == nil {
		t.Fatalf("no run stats for the implementation workflow: %+v", stats.Runs)
	}
	if implementation.FailedRuns != 2 {
		t.Fatalf("implementation FailedRuns = %d, want 2", implementation.FailedRuns)
	}
	if implementation.InfraFailedRuns != 1 {
		t.Fatalf("implementation InfraFailedRuns = %d, want 1", implementation.InfraFailedRuns)
	}
	// Only the work failure remains in the denominator, and no run of this
	// workflow completed — so 0 of 1, which is an honest zero rather than the
	// libelous 0-of-2 the pre-fix rollup reported.
	if implementation.SuccessRate != 0 {
		t.Fatalf("implementation SuccessRate = %v, want 0 over the single work verdict", implementation.SuccessRate)
	}
}

// TestSuccessRateCountsUnclassifiedTerminalsAsWorkFailures pins the
// backward-compatible half of #3364: a run journaled before the terminal
// classification existed carries no runner.errorClass on its run_failed row,
// and must keep counting as a work failure. The split is additive — it never
// reinterprets history it cannot read.
func TestSuccessRateCountsUnclassifiedTerminalsAsWorkFailures(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	db := openTestDB(t, tmp)
	ctx := context.Background()

	legacy := writeTerminalDispositionRun(t, runsDir, fixtureRunID,
		telemetry.ErrCodeCredentialUnavailable, "", fixtureStart)
	if err := db.IngestRun(ctx, legacy); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	summary, err := db.InstanceSummaryStats(ctx, time.Time{})
	if err != nil {
		t.Fatalf("InstanceSummaryStats: %v", err)
	}
	if summary.FailedRuns != 1 {
		t.Fatalf("FailedRuns = %d, want 1", summary.FailedRuns)
	}
	if summary.InfraFailedRuns != 0 {
		t.Fatalf("InfraFailedRuns = %d, want 0 — an unclassified legacy terminal must not be reinterpreted", summary.InfraFailedRuns)
	}
	if summary.SuccessRate != 0 {
		t.Fatalf("SuccessRate = %v, want 0 (the legacy terminal stays in the denominator)", summary.SuccessRate)
	}
}
