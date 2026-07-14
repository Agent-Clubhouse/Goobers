package rollup

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// seedDeployRun writes a run with a "build" stage that always succeeds and
// a "deploy" stage whose outcome is deploySucceeds — unlike seedStatsRun
// (whose second stage either doesn't exist or always fails), this lets a
// test control a stage's failure RATE, not just presence/absence.
func seedDeployRun(t *testing.T, runsDir, runID, workflow string, deploySucceeds bool, startedAt time.Time) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: "+workflow))

	deployStatus, runStatus := "success", "completed"
	if !deploySucceeds {
		deployStatus, runStatus = "failure", "failed"
	}
	lines := []string{
		eventLine(1, startedAt, `"type":"run.started"`),
		eventLine(2, startedAt.Add(time.Second), `"type":"stage.started","stage":"build","attempt":1`),
		eventLine(3, startedAt.Add(2*time.Second), `"type":"stage.finished","stage":"build","attempt":1,"status":"success"`),
		eventLine(4, startedAt.Add(3*time.Second), `"type":"stage.started","stage":"deploy","attempt":1`),
		eventLine(5, startedAt.Add(4*time.Second), fmt.Sprintf(`"type":"stage.finished","stage":"deploy","attempt":1,"status":%q`, deployStatus)),
		eventLine(6, startedAt.Add(5*time.Second), fmt.Sprintf(`"type":"run.finished","status":%q`, runStatus)),
	}
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
}

// seedGateRun writes a run with one gate.evaluated event carrying
// runner:{repassAttempt, escalated} — the shape #128 made queryable —
// for gate-noise detection tests.
func seedGateRun(t *testing.T, runsDir, runID, workflow, verdict string, escalated bool, startedAt time.Time) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: "+workflow))

	lines := []string{
		eventLine(1, startedAt, `"type":"run.started"`),
		eventLine(2, startedAt.Add(time.Second), fmt.Sprintf(
			`"type":"gate.evaluated","gate":"review","verdict":"%s","target":"x","runner":{"repassAttempt":1,"escalated":%t}`,
			verdict, escalated)),
		eventLine(3, startedAt.Add(2*time.Second), `"type":"run.finished","status":"completed"`),
	}
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
}

func TestDetectStageFailureRateThresholdBoundary(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// "deploy" stage: 3 failures / 10 attempts = 30% failure rate — exactly
	// at the default 0.3 threshold, so it must be flagged (>=, not >).
	for i := 0; i < 7; i++ {
		seedDeployRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", true, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 7; i < 10; i++ {
		seedDeployRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", false, base.Add(time.Duration(i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var deploy *Finding
	for i := range findings {
		if findings[i].Kind == FindingStageFailureRate && findings[i].Subject == "deploy" {
			deploy = &findings[i]
		}
	}
	if deploy == nil {
		t.Fatalf("deploy stage not flagged at exactly the 0.3 threshold, findings: %+v", findings)
	}
	if got := deploy.Metrics["failureRate"]; got < 0.29 || got > 0.31 {
		t.Errorf("failureRate = %v, want ~0.3", got)
	}
	if len(deploy.FlaggedRuns) != 3 {
		t.Errorf("FlaggedRuns = %d, want 3", len(deploy.FlaggedRuns))
	}

	// Just under: 2/10 = 20% must NOT be flagged.
	tmp2 := t.TempDir()
	runsDir2 := filepath.Join(tmp2, "runs")
	for i := 0; i < 8; i++ {
		seedDeployRun(t, runsDir2, fmt.Sprintf("%032d", i), "implement", true, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 8; i < 10; i++ {
		seedDeployRun(t, runsDir2, fmt.Sprintf("%032d", i), "implement", false, base.Add(time.Duration(i)*time.Hour))
	}
	db2 := openTestDB(t, tmp2)
	seedAndIngest(t, db2, runsDir2)
	findings2, err := db2.Detect(DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, f := range findings2 {
		if f.Kind == FindingStageFailureRate && f.Subject == "deploy" {
			t.Fatalf("deploy flagged at 20%% failure rate, want no finding below the 30%% threshold: %+v", f)
		}
	}
}

func TestDetectStageFailureRateRequiresMinSamples(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// 1 failure / 1 attempt = 100% failure rate, but below MinSamples (5) —
	// must not be flagged (avoids noise from a single bad run).
	seedStatsRun(t, runsDir, fmt.Sprintf("%032d", 0), "implement", "failed", base, true, "provider.rate_limit")

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, f := range findings {
		if f.Kind == FindingStageFailureRate {
			t.Fatalf("stage flagged below MinSamples: %+v", f)
		}
	}
}

func TestDetectErrorSignatureThreshold(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// "provider.rate_limit" recurs exactly 5 times (the default threshold).
	for i := 0; i < 5; i++ {
		seedStatsRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", "failed", base.Add(time.Duration(i)*time.Hour), true, "provider.rate_limit")
	}
	// A different code occurs only twice — must not be flagged.
	for i := 5; i < 7; i++ {
		seedStatsRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", "failed", base.Add(time.Duration(i)*time.Hour), true, "harness.crash")
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var rateLimit, crash *Finding
	for i := range findings {
		switch findings[i].Subject {
		case "provider.rate_limit":
			rateLimit = &findings[i]
		case "harness.crash":
			crash = &findings[i]
		}
	}
	if rateLimit == nil || rateLimit.Kind != FindingErrorSignature {
		t.Fatalf("provider.rate_limit not flagged at count=5, findings: %+v", findings)
	}
	if rateLimit.Metrics["count"] != 5 {
		t.Errorf("count = %v, want 5", rateLimit.Metrics["count"])
	}
	if len(rateLimit.FlaggedRuns) != 5 {
		t.Errorf("FlaggedRuns = %d, want 5", len(rateLimit.FlaggedRuns))
	}
	if crash != nil {
		t.Fatalf("harness.crash flagged at count=2, want no finding below threshold 5: %+v", crash)
	}
}

func TestDetectGateNeverFails(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	for i := 0; i < 5; i++ {
		seedGateRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", "pass", false, base.Add(time.Duration(i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var never *Finding
	for i := range findings {
		if findings[i].Kind == FindingGateNeverFails {
			never = &findings[i]
		}
	}
	if never == nil || never.Subject != "review" {
		t.Fatalf("review gate not flagged as never-fails, findings: %+v", findings)
	}
	if never.Metrics["totalEvaluations"] != 5 {
		t.Errorf("totalEvaluations = %v, want 5", never.Metrics["totalEvaluations"])
	}
	if len(never.FlaggedRuns) != 5 {
		t.Errorf("FlaggedRuns = %d, want 5", len(never.FlaggedRuns))
	}
}

func TestDetectGateNeverFailsRequiresMinEvaluations(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// Only 2 evaluations, below MinGateEvaluations (5) — must not flag.
	for i := 0; i < 2; i++ {
		seedGateRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", "pass", false, base.Add(time.Duration(i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, f := range findings {
		if f.Kind == FindingGateNeverFails {
			t.Fatalf("gate flagged below MinGateEvaluations: %+v", f)
		}
	}
}

func TestDetectGateRepassChurn(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// 5 evaluations, 2 escalated = 40% escalation rate — above the default
	// 0.2 threshold.
	for i := 0; i < 3; i++ {
		seedGateRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", "pass", false, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 3; i < 5; i++ {
		seedGateRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", "fail", true, base.Add(time.Duration(i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var churn *Finding
	for i := range findings {
		if findings[i].Kind == FindingGateRepassChurn {
			churn = &findings[i]
		}
	}
	if churn == nil || churn.Subject != "review" {
		t.Fatalf("review gate not flagged for repass churn, findings: %+v", findings)
	}
	if got := churn.Metrics["escalationRate"]; got < 0.39 || got > 0.41 {
		t.Errorf("escalationRate = %v, want ~0.4", got)
	}
	if len(churn.FlaggedRuns) != 2 {
		t.Errorf("FlaggedRuns = %d, want 2 (only the escalated evaluations)", len(churn.FlaggedRuns))
	}
}

func TestDetectCoverageGaps(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// "implement" ran with only a "build" stage attempt — "deploy" is
	// defined but never reached. "nominate" is defined but never ran at
	// all.
	seedStatsRun(t, runsDir, fmt.Sprintf("%032d", 0), "implement", "completed", base, false, "")

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(DetectRequest{
		Coverage: CoverageRequest{
			Workflows: map[string][]string{
				"implement": {"build", "deploy"},
				"nominate":  {"scan"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	var sawUntriggered, sawUnreached bool
	for _, f := range findings {
		switch {
		case f.Kind == FindingWorkflowUntriggered && f.Subject == "nominate":
			sawUntriggered = true
			if f.FlaggedRuns != nil {
				t.Errorf("untriggered finding carries FlaggedRuns, want nil: %+v", f)
			}
		case f.Kind == FindingStageUnreached && f.Subject == "implement/deploy":
			sawUnreached = true
		case f.Kind == FindingStageUnreached && f.Subject == "implement/build":
			t.Errorf("build stage was reached, must not be flagged: %+v", f)
		case f.Kind == FindingWorkflowUntriggered && f.Subject == "implement":
			t.Errorf("implement workflow ran, must not be flagged untriggered: %+v", f)
		}
	}
	if !sawUntriggered {
		t.Fatalf("nominate workflow not flagged as untriggered, findings: %+v", findings)
	}
	if !sawUnreached {
		t.Fatalf("implement/deploy stage not flagged as unreached, findings: %+v", findings)
	}
}

// TestDetectIsDeterministic proves Detect's output is stable for a fixed
// telemetry.db snapshot — T2's own test-plan requirement ("artifact output
// is deterministic for a fixed input").
func TestDetectIsDeterministic(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	for i := 0; i < 5; i++ {
		seedStatsRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", "failed", base.Add(time.Duration(i)*time.Hour), true, "provider.rate_limit")
	}
	for i := 5; i < 10; i++ {
		seedGateRun(t, runsDir, fmt.Sprintf("%032d", i), "implement", "pass", false, base.Add(time.Duration(i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	req := DetectRequest{
		Coverage: CoverageRequest{
			Workflows: map[string][]string{"implement": {"build", "deploy"}, "nominate": nil},
		},
	}
	first, err := db.Detect(req)
	if err != nil {
		t.Fatalf("Detect (1st): %v", err)
	}
	second, err := db.Detect(req)
	if err != nil {
		t.Fatalf("Detect (2nd): %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Detect is not deterministic:\n1st: %+v\n2nd: %+v", first, second)
	}
	if len(first) == 0 {
		t.Fatal("expected at least one finding to compare")
	}
}

// TestDetectRedactCanary mirrors TestAggregateQueriesRedactCanary — proves
// the findings layer doesn't reintroduce a secret leak by surfacing a field
// that skipped the ingest-time redaction pass.
func TestDetectRedactCanary(t *testing.T) {
	const canary = "ghp_0123456789abcdefghijklmnopqrstuvwx"
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart
	for i := 0; i < 5; i++ {
		dir := filepath.Join(runsDir, fmt.Sprintf("%032d", i))
		mustMkdirAll(t, dir)
		mustWriteFile(t, filepath.Join(dir, fileRunYAML), minimalRunYAML(fmt.Sprintf("%032d", i), base.Add(time.Duration(i)*time.Hour)))
		lines := []string{
			eventLine(1, base.Add(time.Duration(i)*time.Hour), `"type":"run.started"`),
			eventLine(2, base.Add(time.Duration(i)*time.Hour+time.Second), `"type":"stage.started","stage":"s","attempt":1`),
			eventLine(3, base.Add(time.Duration(i)*time.Hour+2*time.Second), `"type":"error","stage":"s","attempt":1,"error":{"code":"harness.failure","message":"leaked `+canary+`"}`),
			eventLine(4, base.Add(time.Duration(i)*time.Hour+3*time.Second), `"type":"stage.finished","stage":"s","attempt":1,"status":"failure"`),
			eventLine(5, base.Add(time.Duration(i)*time.Hour+4*time.Second), `"type":"run.finished","status":"failed"`),
		}
		mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, f := range findings {
		if strings.Contains(f.Subject, canary) {
			t.Fatalf("canary leaked into finding subject: %+v", f)
		}
	}
}
