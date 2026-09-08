package rollup

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Initial dispatch failures have no attemptClass; their recoveries start infra.
// A policy failure can also happen on an infra-started successor. Only the
// outcome metadata distinguishes these cases.
func TestStageHealthUsesFailureOutcomeAndCountsRecovery(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	for i, tc := range []struct {
		stage, class, retryClass, errorClass string
		recovered                            bool
	}{
		{"recovered", "", "infra", "infra", true},
		{"exhausted", "", "infra", "infra", false},
		{"policy", "", "policy", "executor", false},
		{"policy-after-infra", "infra", "policy", "executor", false},
	} {
		runID := fmt.Sprintf("%032d", i+1)
		dir := filepath.Join(runsDir, runID)
		mustMkdirAll(t, dir)
		mustWriteFile(t, filepath.Join(dir, fileRunYAML), minimalRunYAML(runID, fixtureStart))
		lines := []string{
			eventLine(1, fixtureStart, `"type":"run.started"`),
			eventLine(2, fixtureStart.Add(time.Second), fmt.Sprintf(`"type":"stage.started","stage":%q,"attempt":1,"attemptClass":%q`, tc.stage, tc.class)),
			eventLine(3, fixtureStart.Add(2*time.Second), fmt.Sprintf(`"type":"error","stage":%q,"attempt":1,"attemptClass":%q,"error":{"code":"executor_error"},"runner":{"retryFailureClass":%q,"errorCode":%q,"errorClass":%q}`, tc.stage, tc.class, tc.retryClass, map[bool]string{true: "infra.failure", false: "executor_error"}[tc.errorClass == "infra"], tc.errorClass)),
		}
		if tc.recovered {
			lines = append(lines,
				eventLine(4, fixtureStart.Add(3*time.Second), fmt.Sprintf(`"type":"stage.started","stage":%q,"attempt":2,"attemptClass":"infra"`, tc.stage)),
				eventLine(5, fixtureStart.Add(4*time.Second), fmt.Sprintf(`"type":"stage.finished","stage":%q,"attempt":2,"attemptClass":"infra","status":"success"`, tc.stage)),
				eventLine(6, fixtureStart.Add(5*time.Second), `"type":"run.finished","status":"completed"`))
		} else {
			lines = append(lines,
				eventLine(4, fixtureStart.Add(3*time.Second), fmt.Sprintf(`"type":"error","error":{"code":"run_failed"},"runner":{"errorClass":%q}`, tc.errorClass)),
				eventLine(5, fixtureStart.Add(4*time.Second), `"type":"run.finished","status":"failed"`))
		}
		mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
	}
	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	stats, err := db.Stats(context.Background(), StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byStage := map[string]StageStats{}
	for _, s := range stats.Stages {
		byStage[s.Stage] = s
	}
	if s := byStage["recovered"]; s.TotalAttempts != 1 || s.SucceededAttempts != 1 || s.FailedAttempts != 0 || s.SuccessRate != 1 {
		t.Fatalf("recovered = %+v", s)
	}
	if s, ok := byStage["exhausted"]; ok {
		t.Fatalf("infrastructure counted as work: %+v", s)
	}
	for _, stage := range []string{"policy", "policy-after-infra"} {
		if s := byStage[stage]; s.TotalAttempts != 1 || s.FailedAttempts != 1 {
			t.Fatalf("%s = %+v", stage, s)
		}
	}
	summary, err := db.InstanceSummaryStats(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 1 || summary.FailedRuns != 3 || summary.InfraFailedRuns != 1 || summary.SuccessRate != 1.0/3.0 {
		t.Fatalf("summary = %+v", summary)
	}
}
