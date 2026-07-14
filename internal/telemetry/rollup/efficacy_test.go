package rollup

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedDigestRun writes a minimal run under an explicit workflow_digest and
// status, for before/after efficacy comparison and digest-history tests —
// seedStatsRun (aggregates_test.go) doesn't let a caller pin the digest,
// since T2's tests never needed to distinguish one digest from another.
func seedDigestRun(t *testing.T, runsDir, runID, workflow, digest, runStatus string, startedAt time.Time) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, dir)
	runYAML := fmt.Sprintf(`schema: goobers.dev/journal/run/v1
runId: %s
workflow: %s
workflowVersion: 1
workflowDigest: %s
gaggle: web
trigger:
  kind: schedule
  ref: "*/5 * * * *"
startedAt: %s
`, runID, workflow, digest, startedAt.UTC().Format(time.RFC3339))
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), runYAML)

	lines := []string{
		eventLine(1, startedAt, `"type":"run.started"`),
		eventLine(2, startedAt.Add(time.Second), `"type":"stage.started","stage":"scan","attempt":1`),
		eventLine(3, startedAt.Add(2*time.Second), fmt.Sprintf(`"type":"stage.finished","stage":"scan","attempt":1,"status":%q`, statusForRun(runStatus))),
		eventLine(4, startedAt.Add(3*time.Second), fmt.Sprintf(`"type":"run.finished","status":%q`, runStatus)),
	}
	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
}

// statusForRun maps a run.finished status to its single stage's status —
// good enough for efficacy tests, which only assert on run-level
// completed/failed counts, not stage detail.
func statusForRun(runStatus string) string {
	if runStatus == runStatusCompleted {
		return "success"
	}
	return "failure"
}

func TestDigestHistoryDetectsTransitions(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// digestA x3, then digestB x2 — one transition (A -> B).
	seedDigestRun(t, runsDir, fmt.Sprintf("%032d", 0), "nominate", "sha256:aaaa", runStatusCompleted, base)
	seedDigestRun(t, runsDir, fmt.Sprintf("%032d", 1), "nominate", "sha256:aaaa", runStatusCompleted, base.Add(time.Hour))
	seedDigestRun(t, runsDir, fmt.Sprintf("%032d", 2), "nominate", "sha256:aaaa", runStatusCompleted, base.Add(2*time.Hour))
	seedDigestRun(t, runsDir, fmt.Sprintf("%032d", 3), "nominate", "sha256:bbbb", runStatusCompleted, base.Add(3*time.Hour))
	seedDigestRun(t, runsDir, fmt.Sprintf("%032d", 4), "nominate", "sha256:bbbb", runStatusCompleted, base.Add(4*time.Hour))

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	changes, err := db.DigestHistory("nominate")
	if err != nil {
		t.Fatalf("DigestHistory: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want exactly 1 transition", changes)
	}
	c := changes[0]
	if c.FromDigest != "sha256:aaaa" || c.ToDigest != "sha256:bbbb" {
		t.Errorf("transition = %+v, want aaaa -> bbbb", c)
	}
	if !c.ChangedAt.Equal(base.Add(3 * time.Hour)) {
		t.Errorf("ChangedAt = %v, want %v (first run under the new digest)", c.ChangedAt, base.Add(3*time.Hour))
	}
}

func TestDigestHistoryNoTransitionsWithOneDigest(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart
	seedDigestRun(t, runsDir, fmt.Sprintf("%032d", 0), "nominate", "sha256:aaaa", runStatusCompleted, base)
	seedDigestRun(t, runsDir, fmt.Sprintf("%032d", 1), "nominate", "sha256:aaaa", runStatusCompleted, base.Add(time.Hour))

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	changes, err := db.DigestHistory("nominate")
	if err != nil {
		t.Fatalf("DigestHistory: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none (only one digest ever seen)", changes)
	}
}

func TestAssessEfficacyHelped(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// Before: 5 runs, 3 failed (60% failure rate).
	for i := 0; i < 2; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("b%031d", i), "nominate", "sha256:aaaa", runStatusCompleted, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 2; i < 5; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("b%031d", i), "nominate", "sha256:aaaa", runStatusFailed, base.Add(time.Duration(i)*time.Hour))
	}
	// After: 5 runs, 0 failed (0% failure rate) — a clear improvement.
	for i := 0; i < 5; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("a%031d", i), "nominate", "sha256:bbbb", runStatusCompleted, base.Add(time.Duration(10+i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	result, err := db.AssessEfficacy(EfficacyRequest{
		Workflow: "nominate", OldDigest: "sha256:aaaa", NewDigest: "sha256:bbbb",
		Thresholds: DefaultEfficacyThresholds(),
	})
	if err != nil {
		t.Fatalf("AssessEfficacy: %v", err)
	}
	if result.Verdict != EfficacyHelped {
		t.Fatalf("Verdict = %q, want helped: %+v", result.Verdict, result)
	}
	if result.Before.FailedRuns != 3 || result.After.FailedRuns != 0 {
		t.Errorf("Before/After = %+v / %+v", result.Before, result.After)
	}
	if result.FailureRateDelta >= 0 {
		t.Errorf("FailureRateDelta = %v, want negative (improvement)", result.FailureRateDelta)
	}
}

func TestAssessEfficacyRegressed(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// Before: 5 runs, all completed (0% failure).
	for i := 0; i < 5; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("b%031d", i), "nominate", "sha256:aaaa", runStatusCompleted, base.Add(time.Duration(i)*time.Hour))
	}
	// After: 5 runs, 4 failed (80% failure) — a clear regression.
	seedDigestRun(t, runsDir, fmt.Sprintf("a%031d", 0), "nominate", "sha256:bbbb", runStatusCompleted, base.Add(10*time.Hour))
	for i := 1; i < 5; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("a%031d", i), "nominate", "sha256:bbbb", runStatusFailed, base.Add(time.Duration(10+i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	result, err := db.AssessEfficacy(EfficacyRequest{
		Workflow: "nominate", OldDigest: "sha256:aaaa", NewDigest: "sha256:bbbb",
		Thresholds: DefaultEfficacyThresholds(),
	})
	if err != nil {
		t.Fatalf("AssessEfficacy: %v", err)
	}
	if result.Verdict != EfficacyRegressed {
		t.Fatalf("Verdict = %q, want regressed: %+v", result.Verdict, result)
	}
	if result.FailureRateDelta <= 0 {
		t.Errorf("FailureRateDelta = %v, want positive (regression)", result.FailureRateDelta)
	}
}

func TestAssessEfficacyNoChangeWithinThreshold(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// Before: 10 runs, 2 failed (20% failure). After: 10 runs, 3 failed
	// (30% failure) — a 10-point delta is above the default 5-point
	// threshold... use a SMALLER delta instead: before 20%, after 22% (not
	// achievable with small integer counts), so use identical rates: both
	// 20% failure — zero delta, unambiguously "no-change".
	for i := 0; i < 8; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("b%031d", i), "nominate", "sha256:aaaa", runStatusCompleted, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 8; i < 10; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("b%031d", i), "nominate", "sha256:aaaa", runStatusFailed, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 0; i < 8; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("a%031d", i), "nominate", "sha256:bbbb", runStatusCompleted, base.Add(time.Duration(20+i)*time.Hour))
	}
	for i := 8; i < 10; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("a%031d", i), "nominate", "sha256:bbbb", runStatusFailed, base.Add(time.Duration(20+i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	result, err := db.AssessEfficacy(EfficacyRequest{
		Workflow: "nominate", OldDigest: "sha256:aaaa", NewDigest: "sha256:bbbb",
		Thresholds: DefaultEfficacyThresholds(),
	})
	if err != nil {
		t.Fatalf("AssessEfficacy: %v", err)
	}
	if result.Verdict != EfficacyNoChange {
		t.Fatalf("Verdict = %q, want no-change: %+v", result.Verdict, result)
	}
	if result.FailureRateDelta != 0 {
		t.Errorf("FailureRateDelta = %v, want 0 (identical rates)", result.FailureRateDelta)
	}
}

func TestAssessEfficacyInsufficientData(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// Only 2 runs per segment, below the default MinSamples=5.
	for i := 0; i < 2; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("b%031d", i), "nominate", "sha256:aaaa", runStatusCompleted, base.Add(time.Duration(i)*time.Hour))
		seedDigestRun(t, runsDir, fmt.Sprintf("a%031d", i), "nominate", "sha256:bbbb", runStatusFailed, base.Add(time.Duration(10+i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	result, err := db.AssessEfficacy(EfficacyRequest{
		Workflow: "nominate", OldDigest: "sha256:aaaa", NewDigest: "sha256:bbbb",
		Thresholds: DefaultEfficacyThresholds(),
	})
	if err != nil {
		t.Fatalf("AssessEfficacy: %v", err)
	}
	if result.Verdict != EfficacyInsufficientData {
		t.Fatalf("Verdict = %q, want insufficient-data: %+v", result.Verdict, result)
	}
	if result.FailureRateDelta != 0 {
		t.Errorf("FailureRateDelta = %v, want 0 (no verdict rendered)", result.FailureRateDelta)
	}
}

func TestAssessLatestEfficacyFindsMostRecentTransition(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// Two transitions: aaaa -> bbbb -> cccc. AssessLatestEfficacy must
	// compare bbbb (before) vs cccc (after), not aaaa vs bbbb.
	for i := 0; i < 5; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("a%031d", i), "nominate", "sha256:aaaa", runStatusFailed, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 0; i < 5; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("b%031d", i), "nominate", "sha256:bbbb", runStatusCompleted, base.Add(time.Duration(10+i)*time.Hour))
	}
	for i := 0; i < 5; i++ {
		seedDigestRun(t, runsDir, fmt.Sprintf("c%031d", i), "nominate", "sha256:cccc", runStatusFailed, base.Add(time.Duration(20+i)*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	result, err := db.AssessLatestEfficacy("nominate", time.Time{}, DefaultEfficacyThresholds())
	if err != nil {
		t.Fatalf("AssessLatestEfficacy: %v", err)
	}
	if result.OldDigest != "sha256:bbbb" || result.NewDigest != "sha256:cccc" {
		t.Fatalf("compared %s -> %s, want bbbb -> cccc (the most recent transition)", result.OldDigest, result.NewDigest)
	}
	if result.Verdict != EfficacyRegressed {
		t.Fatalf("Verdict = %q, want regressed (bbbb all-pass -> cccc all-fail)", result.Verdict)
	}
}

func TestChurnGuardFlagsRepeatedFlipFlops(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// 3 transitions within the window: aaaa->bbbb->cccc->dddd.
	digests := []string{"sha256:aaaa", "sha256:bbbb", "sha256:cccc", "sha256:dddd"}
	seq := 0
	for i, d := range digests {
		seedDigestRun(t, runsDir, fmt.Sprintf("%032d", seq), "nominate", d, runStatusCompleted, base.Add(time.Duration(i)*time.Hour))
		seq++
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	result, err := db.ChurnGuard(ChurnGuardRequest{Workflow: "nominate", MaxChanges: 3})
	if err != nil {
		t.Fatalf("ChurnGuard: %v", err)
	}
	if result.ChangeCount != 3 || !result.Flagged {
		t.Fatalf("result = %+v, want ChangeCount=3 Flagged=true", result)
	}

	// A lower bar of 2 transitions is fine — must not flag.
	notFlagged, err := db.ChurnGuard(ChurnGuardRequest{Workflow: "steady-workflow", MaxChanges: 3})
	if err != nil {
		t.Fatalf("ChurnGuard (no history): %v", err)
	}
	if notFlagged.Flagged || notFlagged.ChangeCount != 0 {
		t.Fatalf("result = %+v, want unflagged for a workflow with no run history at all", notFlagged)
	}
}

func TestChurnGuardRespectsWindow(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// 3 transitions, but only the LAST one falls inside the window.
	digests := []string{"sha256:aaaa", "sha256:bbbb", "sha256:cccc", "sha256:dddd"}
	for i, d := range digests {
		seedDigestRun(t, runsDir, fmt.Sprintf("%032d", i), "nominate", d, runStatusCompleted, base.Add(time.Duration(i)*24*time.Hour))
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	// Window starts right before the last transition (day 3) — only 1
	// change should count, below MaxChanges=2.
	windowed, err := db.ChurnGuard(ChurnGuardRequest{
		Workflow:   "nominate",
		Since:      base.Add(2*24*time.Hour + time.Hour),
		MaxChanges: 2,
	})
	if err != nil {
		t.Fatalf("ChurnGuard: %v", err)
	}
	if windowed.ChangeCount != 1 || windowed.Flagged {
		t.Fatalf("windowed result = %+v, want ChangeCount=1 Flagged=false", windowed)
	}
}
