package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
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

func seedCICheckFailureRun(t *testing.T, runsDir, runID, checkName string, startedAt time.Time) {
	t.Helper()
	seedCICheckFailurePollsRun(t, runsDir, runID, checkName, 1, startedAt)
}

func seedCICheckFailurePollsRun(t *testing.T, runsDir, runID, checkName string, polls int, startedAt time.Time) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), strings.ReplaceAll(
		minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: implementation",
	))
	artifact, err := json.Marshal(map[string]any{"checks": []map[string]string{{
		"name": checkName, "state": "failing", "summary": "TestResume timed out",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(artifact)
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	artifactPath := filepath.Join("artifacts", "sha256", hexDigest[:2], hexDigest[2:])
	mustMkdirAll(t, filepath.Join(dir, filepath.Dir(artifactPath)))
	mustWriteFile(t, filepath.Join(dir, artifactPath), string(artifact))
	refs, err := json.Marshal([]map[string]any{{
		"path": artifactPath, "digest": digest, "size": len(artifact), "mediaType": "application/json",
	}})
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{eventLine(1, startedAt, `"type":"run.started"`)}
	seq := 2
	for attempt := 1; attempt <= polls; attempt++ {
		lines = append(lines,
			eventLine(seq, startedAt.Add(time.Duration(seq-1)*time.Second),
				fmt.Sprintf(`"type":"stage.started","stage":"ci-poll","attempt":%d`, attempt)),
			eventLine(seq+1, startedAt.Add(time.Duration(seq)*time.Second),
				fmt.Sprintf(`"type":"stage.finished","stage":"ci-poll","attempt":%d,"status":"success","outputs":{"ciStatus":"failing"},"artifacts":%s`, attempt, refs)),
		)
		seq += 2
	}
	lines = append(lines, eventLine(seq, startedAt.Add(time.Duration(seq-1)*time.Second),
		`"type":"run.finished","status":"completed"`))
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

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
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
	findings2, err := db2.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
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

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, f := range findings {
		if f.Kind == FindingStageFailureRate {
			t.Fatalf("stage flagged below MinSamples: %+v", f)
		}
	}
}

// appendRunFailedTerminalMarker journals the universal terminal-cause error
// event (internal/runner/run.go's failTerminal/finishStageFailure: Type:
// EventError, Error.Code: "run_failed") that accompanies every failed run's
// real-cause error event — reproducing the exact self-clustering shape
// #3916 is about. seq must be past the run's last seeded event.
func appendRunFailedTerminalMarker(t *testing.T, runsDir, runID string, seq int, ts time.Time) {
	t.Helper()
	path := filepath.Join(runsDir, runID, fileEvents)
	line := eventLine(seq, ts, `"type":"error","error":{"code":"run_failed","message":"run failed"}`) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append run_failed marker: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("append run_failed marker: %v", err)
	}
}

// TestDetectErrorSignaturesExcludeRunFailedSelfCluster is the regression
// test for #3916 defect 1: run_failed is journaled on EVERY failed run
// alongside the real cause, so left unfiltered it self-clusters into its
// own noise finding duplicating almost the same run set as the real cause.
func TestDetectErrorSignaturesExcludeRunFailedSelfCluster(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	for i := 0; i < 5; i++ {
		runID := fmt.Sprintf("%032d", i)
		startedAt := base.Add(time.Duration(i) * time.Hour)
		seedStatsRun(t, runsDir, runID, "implement", "failed", startedAt, true, "provider.rate_limit")
		// seedStatsRun's failing path ends at seq 7 (run.started..run.finished);
		// the terminal marker lands one seq after, exactly where
		// failTerminal/finishStageFailure would append it in a real run.
		appendRunFailedTerminalMarker(t, runsDir, runID, 8, startedAt.Add(10*time.Second))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var rateLimit *Finding
	for i := range findings {
		if findings[i].Kind != FindingErrorSignature {
			continue
		}
		if findings[i].Subject == "run_failed" {
			t.Fatalf("run_failed self-clustered into its own error-signature finding: %+v", findings[i])
		}
		if findings[i].Subject == "provider.rate_limit" {
			rateLimit = &findings[i]
		}
	}
	if rateLimit == nil {
		t.Fatalf("provider.rate_limit (the real cause) was not flagged, findings: %+v", findings)
	}
	if rateLimit.Metrics["count"] != 5 {
		t.Errorf("count = %v, want 5", rateLimit.Metrics["count"])
	}
}

// seedErrorClassOverrideRun writes a minimal failed run whose single error
// event carries the same code as any other caller's but an explicit
// runner.errorClass override — the same mechanism failTerminal uses
// (internal/telemetry/rollup/ingest.go's errorCodeAndClass) — so two runs
// can share a code while landing in different (code, error_class) buckets.
func seedErrorClassOverrideRun(t *testing.T, runsDir, runID string, startedAt time.Time, code, errorClass string) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), strings.ReplaceAll(minimalRunYAML(runID, startedAt), "workflow: wf", "workflow: implement"))
	lines := []string{
		eventLine(1, startedAt, `"type":"run.started"`),
		eventLine(2, startedAt.Add(time.Second), `"type":"stage.started","stage":"deploy","attempt":1`),
		eventLine(3, startedAt.Add(2*time.Second), `"type":"error","stage":"deploy","attempt":1,"error":{"code":"`+code+`","message":"seeded"},"runner":{"errorClass":"`+errorClass+`"}`),
		eventLine(4, startedAt.Add(3*time.Second), `"type":"stage.finished","stage":"deploy","attempt":1,"status":"failure"`),
		eventLine(5, startedAt.Add(4*time.Second), `"type":"run.finished","status":"failed"`),
	}
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
}

// TestDetectErrorSignaturesDistinguishesSameCodeDifferentErrorClass is the
// regression test for #3916 defect 2: two (code, error_class) signatures
// sharing a code were previously indistinguishable on the wire because
// Subject carried only the code — a nominator handed two byte-identical
// Subject rows for genuinely different failure classes.
func TestDetectErrorSignaturesDistinguishesSameCodeDifferentErrorClass(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart
	const sharedCode = "github_auth_failed"

	for i := 0; i < 5; i++ {
		seedErrorClassOverrideRun(t, runsDir, fmt.Sprintf("a%031d", i), base.Add(time.Duration(i)*time.Hour), sharedCode, "app-auth")
	}
	for i := 0; i < 5; i++ {
		seedErrorClassOverrideRun(t, runsDir, fmt.Sprintf("b%031d", i), base.Add(time.Duration(i+10)*time.Hour), sharedCode, "pat-auth")
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	byClass := map[string]*Finding{}
	for i := range findings {
		if findings[i].Kind != FindingErrorSignature || findings[i].Subject != sharedCode {
			continue
		}
		byClass[findings[i].ErrorClass] = &findings[i]
	}
	appAuth, appAuthOK := byClass["app-auth"]
	patAuth, patAuthOK := byClass["pat-auth"]
	if !appAuthOK || !patAuthOK {
		t.Fatalf("expected two distinguishable %s findings (app-auth, pat-auth), got: %+v", sharedCode, findings)
	}
	if appAuth.Subject != sharedCode || patAuth.Subject != sharedCode {
		t.Fatalf("both findings must keep the shared Subject: app-auth=%q pat-auth=%q", appAuth.Subject, patAuth.Subject)
	}
	if len(appAuth.FlaggedRuns) != 5 || len(patAuth.FlaggedRuns) != 5 {
		t.Fatalf("each class's runs must not cross-contaminate: app-auth=%d pat-auth=%d", len(appAuth.FlaggedRuns), len(patAuth.FlaggedRuns))
	}
}

// TestDetectErrorSignatureFlaggedRunsCarryResolvableSeq is the regression
// test for #3916 defect 3: flagged_runs must name the exact run_errors
// event a nominator can cite, not just the run id — errorSignatureRuns
// previously discarded seq entirely (queryRunIDs's shared scan has no seq
// column to give it). This confirms the returned Seq is non-zero AND
// resolves to the actual seeded "error" event in that run's journal, not
// an arbitrary or off-by-one value.
func TestDetectErrorSignatureFlaggedRunsCarryResolvableSeq(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// Alpha-prefixed, not "%032d": a purely-numeric run id can round-trip
	// through SQLite's dynamic column typing with its leading zeros
	// stripped, which is an unrelated pre-existing quirk of the fixture
	// helpers this test must not trip over — it needs the exact id back to
	// open the run's own journal directory below.
	for i := 0; i < 5; i++ {
		seedStatsRun(t, runsDir, fmt.Sprintf("r%031d", i), "implement", "failed", base.Add(time.Duration(i)*time.Hour), true, "provider.rate_limit")
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	var rateLimit *Finding
	for i := range findings {
		if findings[i].Kind == FindingErrorSignature && findings[i].Subject == "provider.rate_limit" {
			rateLimit = &findings[i]
		}
	}
	if rateLimit == nil {
		t.Fatalf("provider.rate_limit not flagged, findings: %+v", findings)
	}
	if len(rateLimit.FlaggedRuns) != 5 {
		t.Fatalf("FlaggedRuns = %d, want 5", len(rateLimit.FlaggedRuns))
	}
	for _, pointer := range rateLimit.FlaggedRuns {
		if pointer.Seq == 0 {
			t.Fatalf("flagged run %s carries no seq: %+v", pointer.RunID, pointer)
		}
		// seedStatsRun's failing path journals the error event at seq 5
		// (run.started=1, stage.started=2, stage.finished=3, stage.started=4,
		// error=5, stage.finished=6, run.finished=7) — verify the returned
		// seq actually names THAT event, not an arbitrary non-zero number.
		events, err := journal.OpenRead(filepath.Join(runsDir, pointer.RunID))
		if err != nil {
			t.Fatalf("open journal for %s: %v", pointer.RunID, err)
		}
		all, err := events.Events()
		if err != nil {
			t.Fatalf("read events for %s: %v", pointer.RunID, err)
		}
		var resolved *journal.Event
		for i := range all {
			if all[i].Seq == pointer.Seq {
				resolved = &all[i]
				break
			}
		}
		if resolved == nil {
			t.Fatalf("seq %d for run %s does not resolve to any journaled event", pointer.Seq, pointer.RunID)
		}
		if resolved.Type != journal.EventError || resolved.Error == nil || resolved.Error.Code != "provider.rate_limit" {
			t.Fatalf("seq %d for run %s resolved to %+v, want the provider.rate_limit error event", pointer.Seq, pointer.RunID, resolved)
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

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
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

func TestDetectCICheckFailureRequiresDistinctRecurringRuns(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart
	seedCICheckFailureRun(t, runsDir, fmt.Sprintf("%032d", 1), "unit-tests", base)
	seedCICheckFailureRun(t, runsDir, fmt.Sprintf("%032d", 2), "unit-tests", base.Add(time.Hour))
	seedCICheckFailureRun(t, runsDir, fmt.Sprintf("%032d", 3), "lint", base.Add(2*time.Hour))

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	thresholds := DefaultThresholds()
	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: thresholds})
	if err != nil {
		t.Fatal(err)
	}
	var recurring *Finding
	for i := range findings {
		if findings[i].Kind == FindingCICheckFailure {
			if findings[i].Subject == "lint" {
				t.Fatalf("single-run CI failure was classified as recurring: %+v", findings[i])
			}
			if findings[i].Subject == "unit-tests" {
				recurring = &findings[i]
			}
		}
	}
	if recurring == nil {
		t.Fatalf("recurring unit-tests failure not found: %+v", findings)
	}
	if recurring.Metrics["distinctRuns"] != 2 || len(recurring.FlaggedRuns) != 2 {
		t.Fatalf("recurring CI finding = %+v, want two distinct evidence runs", recurring)
	}
}

func TestDetectCICheckFailureEvidenceUsesDistinctRuns(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	olderRun := strings.Repeat("a", 32)
	newerRun := strings.Repeat("b", 32)
	seedCICheckFailureRun(t, runsDir, olderRun, "unit-tests", fixtureStart)
	seedCICheckFailurePollsRun(t, runsDir, newerRun, "unit-tests", 3, fixtureStart.Add(time.Hour))

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	thresholds := DefaultThresholds()
	thresholds.MaxFlaggedRuns = 2
	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: thresholds})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.Kind != FindingCICheckFailure || finding.Subject != "unit-tests" {
			continue
		}
		want := []JournalPointer{{RunID: newerRun}, {RunID: olderRun}}
		if !reflect.DeepEqual(finding.FlaggedRuns, want) {
			t.Fatalf("FlaggedRuns = %+v, want distinct runs %+v", finding.FlaggedRuns, want)
		}
		return
	}
	t.Fatalf("recurring unit-tests failure not found: %+v", findings)
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

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
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

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
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

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
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

func TestDetectGateRepassChurnExcludesInfrastructureEscalation(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	for i := 0; i < 5; i++ {
		dir := filepath.Join(runsDir, fmt.Sprintf("%032d", i))
		mustMkdirAll(t, dir)
		runID := fmt.Sprintf("%032d", i)
		mustWriteFile(t, filepath.Join(dir, fileRunYAML), strings.ReplaceAll(minimalRunYAML(runID, base.Add(time.Duration(i)*time.Hour)), "workflow: wf", "workflow: implementation"))
		line := eventLine(2, base.Add(time.Duration(i)*time.Hour), `"type":"gate.evaluated","gate":"local-gate","verdict":"infra","target":"park-escalated","runner":{"escalated":true}`)
		mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join([]string{
			eventLine(1, base.Add(time.Duration(i)*time.Hour), `"type":"run.started"`),
			line,
			eventLine(3, base.Add(time.Duration(i)*time.Hour+time.Second), `"type":"run.finished","status":"completed"`),
		}, "\n")+"\n")
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, finding := range findings {
		if finding.Kind == FindingGateRepassChurn && finding.Subject == "local-gate" {
			t.Fatalf("infrastructure escalation was classified as repass churn: %+v", finding)
		}
	}
	var classifications int
	if err := db.readDB().QueryRow(
		`SELECT COUNT(*) FROM gate_classifications WHERE gate = 'local-gate'`,
	).Scan(&classifications); err != nil {
		t.Fatalf("gate classification count: %v", err)
	}
	if classifications != 5 {
		t.Fatalf("classifications = %d, want one for each run", classifications)
	}
	var nonInfrastructure int
	if err := db.readDB().QueryRow(
		`SELECT COUNT(*) FROM gate_classifications WHERE gate = 'local-gate' AND classification != 'infrastructure'`,
	).Scan(&nonInfrastructure); err != nil {
		t.Fatalf("gate classification query: %v", err)
	}
	if nonInfrastructure != 0 {
		t.Fatalf("non-infrastructure classifications = %d, want 0", nonInfrastructure)
	}
}

func TestLegacyGateEscalationsAreClassifiedFromJournalOutcome(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	runID := "legacy-infrastructure-run"
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	base := fixtureStart
	mustWriteFile(t, filepath.Join(dir, fileRunYAML),
		strings.ReplaceAll(minimalRunYAML(runID, base), "workflow: wf", "workflow: implementation"))
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join([]string{
		eventLine(1, base, `"type":"run.started"`),
		eventLine(2, base.Add(time.Second), `"type":"gate.evaluated","gate":"local-gate","verdict":"infra","target":"park-escalated","runner":{"escalated":true}`),
	}, "\n")+"\n")

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)
	var classification, reason string
	if err := db.readDB().QueryRow(
		`SELECT classification, reason FROM gate_classifications WHERE run_id = ? AND seq = 2`,
		runID,
	).Scan(&classification, &reason); err != nil {
		t.Fatalf("legacy gate classification query: %v", err)
	}
	if classification != "infrastructure" || reason != "INFRASTRUCTURE_REPASS_BUDGET_EXHAUSTED" {
		t.Fatalf("legacy classification = %q/%q, want infrastructure/INFRASTRUCTURE_REPASS_BUDGET_EXHAUSTED", classification, reason)
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

	findings, err := db.Detect(context.Background(), DetectRequest{
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
			if len(f.FlaggedRuns) != 0 {
				t.Errorf("untriggered finding carries FlaggedRuns, want empty: %+v", f)
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
	first, err := db.Detect(context.Background(), req)
	if err != nil {
		t.Fatalf("Detect (1st): %v", err)
	}
	second, err := db.Detect(context.Background(), req)
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

	findings, err := db.Detect(context.Background(), DetectRequest{Thresholds: DefaultThresholds()})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for _, f := range findings {
		if strings.Contains(f.Subject, canary) {
			t.Fatalf("canary leaked into finding subject: %+v", f)
		}
	}
}
