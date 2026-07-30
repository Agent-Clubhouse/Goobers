package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

// agentSpanFixture is one agentic task span to attach to an
// seedEffectiveVersionRun fixture — its own stage, so multiple spans in one
// run never contend over traversal matching.
type agentSpanFixture struct {
	model, harness string
}

// seedEffectiveVersionRun writes a run pinned to workflowDigest/gooberDigest
// (empty gooberDigest omits the run_goober_digests satellite row) with one
// agentic task span per entry in spans (empty means a non-agentic run).
func seedEffectiveVersionRun(
	t *testing.T,
	runsDir, runID, workflow, workflowDigest, gooberDigest, runStatus string,
	startedAt time.Time,
	spans ...agentSpanFixture,
) {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	mustMkdirAll(t, filepath.Join(dir, dirSpans))

	runYAML := fmt.Sprintf(`schema: goobers.dev/journal/run/v1
runId: %s
workflow: %s
workflowVersion: 1
workflowDigest: %s
gooberDigest: %s
gaggle: web
trigger:
  kind: schedule
  ref: "*/5 * * * *"
startedAt: %s
`, runID, workflow, workflowDigest, gooberDigest, startedAt.UTC().Format(time.RFC3339))
	mustWriteFile(t, filepath.Join(dir, fileRunYAML), runYAML)

	seq := 1
	cursor := startedAt
	lines := []string{eventLine(seq, cursor, `"type":"run.started"`)}
	var spanLines []string
	for i, sp := range spans {
		stage := fmt.Sprintf("agent%d", i)
		seq++
		cursor = cursor.Add(time.Second)
		started := cursor
		lines = append(lines, eventLine(seq, cursor, fmt.Sprintf(`"type":"stage.started","stage":%q,"attempt":1`, stage)))

		seq++
		cursor = cursor.Add(time.Second)
		lines = append(lines, eventLine(seq, cursor, fmt.Sprintf(`"type":"stage.finished","stage":%q,"attempt":1,"status":%q`, stage, statusForRun(runStatus))))

		record := telemetry.SpanRecord{
			Schema:    telemetry.SpanSchema,
			TraceID:   runID,
			SpanID:    fmt.Sprintf("%016x", i+1),
			Name:      "task/" + stage,
			Kind:      telemetry.SpanKindTask,
			StartTime: started,
			EndTime:   cursor,
			Status:    "ok",
			Attributes: map[string]string{
				telemetry.AttrStage:          stage,
				telemetry.AttrAttemptNumber:  "1",
				telemetry.AttrModel:          sp.model,
				telemetry.AttrHarnessVersion: sp.harness,
			},
		}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal span fixture: %v", err)
		}
		spanLines = append(spanLines, string(data))
	}
	if len(spans) == 0 {
		seq++
		cursor = cursor.Add(time.Second)
		lines = append(lines, eventLine(seq, cursor, `"type":"stage.started","stage":"scan","attempt":1`))
		seq++
		cursor = cursor.Add(time.Second)
		lines = append(lines, eventLine(seq, cursor, fmt.Sprintf(`"type":"stage.finished","stage":"scan","attempt":1,"status":%q`, statusForRun(runStatus))))
	}
	seq++
	cursor = cursor.Add(time.Second)
	lines = append(lines, eventLine(seq, cursor, fmt.Sprintf(`"type":"run.finished","status":%q`, runStatus)))

	mustWriteFile(t, filepath.Join(dir, fileEvents), strings.Join(lines, "\n")+"\n")
	if len(spanLines) > 0 {
		mustWriteFile(t, filepath.Join(dir, dirSpans, fileSpans), strings.Join(spanLines, "\n")+"\n")
	}
}

func TestEffectiveVersionHashDiffersPerAxis(t *testing.T) {
	base := EffectiveVersion{WorkflowDigest: "sha256:aaaa", GooberDigest: "sha256:goob1", Model: "claude-sonnet-5", HarnessVersion: "1.0.0"}
	variants := []EffectiveVersion{
		{WorkflowDigest: "sha256:bbbb", GooberDigest: base.GooberDigest, Model: base.Model, HarnessVersion: base.HarnessVersion},
		{WorkflowDigest: base.WorkflowDigest, GooberDigest: "sha256:goob2", Model: base.Model, HarnessVersion: base.HarnessVersion},
		{WorkflowDigest: base.WorkflowDigest, GooberDigest: base.GooberDigest, Model: "claude-opus-5", HarnessVersion: base.HarnessVersion},
		{WorkflowDigest: base.WorkflowDigest, GooberDigest: base.GooberDigest, Model: base.Model, HarnessVersion: "1.0.1"},
	}
	baseHash := base.Hash()
	for i, v := range variants {
		if v.Hash() == baseHash {
			t.Errorf("variant %d (changed one axis) hashed identically to base — cohort key must change on any axis", i)
		}
	}
	// Identical inputs must hash identically (determinism).
	again := EffectiveVersion{WorkflowDigest: base.WorkflowDigest, GooberDigest: base.GooberDigest, Model: base.Model, HarnessVersion: base.HarnessVersion}
	if base.Hash() != again.Hash() {
		t.Fatalf("EffectiveVersion.Hash() is not deterministic")
	}
}

func TestDigestHistoryByEffectiveVersionDetectsModelOnlyTransition(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// Same workflow+goober digest throughout — only the model changes.
	// A plain workflow_digest-keyed view (DigestHistory) would see this as
	// one continuous cohort; EffectiveVersion must not.
	seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("%032d", 0), "tutor", "sha256:aaaa", "sha256:goob1", runStatusCompleted, base,
		agentSpanFixture{model: "claude-sonnet-5", harness: "1.0.0"})
	seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("%032d", 1), "tutor", "sha256:aaaa", "sha256:goob1", runStatusCompleted, base.Add(time.Hour),
		agentSpanFixture{model: "claude-sonnet-5", harness: "1.0.0"})
	seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("%032d", 2), "tutor", "sha256:aaaa", "sha256:goob1", runStatusCompleted, base.Add(2*time.Hour),
		agentSpanFixture{model: "claude-opus-5", harness: "1.0.0"})

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	digestChanges, err := db.DigestHistory(context.Background(), "tutor")
	if err != nil {
		t.Fatalf("DigestHistory: %v", err)
	}
	if len(digestChanges) != 0 {
		t.Fatalf("DigestHistory (workflow_digest only) = %+v, want none — workflow_digest never changed", digestChanges)
	}

	changes, err := db.DigestHistoryByEffectiveVersion(context.Background(), "tutor")
	if err != nil {
		t.Fatalf("DigestHistoryByEffectiveVersion: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want exactly 1 transition (model-only)", changes)
	}
	if changes[0].FromVersion.Model != "claude-sonnet-5" || changes[0].ToVersion.Model != "claude-opus-5" {
		t.Errorf("transition = %+v, want sonnet -> opus", changes[0])
	}
}

func TestEffectiveVersionExcludesMixedModelRuns(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// A run whose agentic spans disagree on model has no defined
	// EffectiveVersion — it must not pool into either side of a comparison.
	seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("%032d", 0), "tutor", "sha256:aaaa", "sha256:goob1", runStatusCompleted, base,
		agentSpanFixture{model: "claude-sonnet-5", harness: "1.0.0"},
		agentSpanFixture{model: "claude-opus-5", harness: "1.0.0"})

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	rows, err := db.effectiveVersionRowsForGaggle(context.Background(), "", "tutor", time.Time{})
	if err != nil {
		t.Fatalf("effectiveVersionRows: %v", err)
	}
	if len(rows) != 1 || !rows[0].Excluded {
		t.Fatalf("rows = %+v, want exactly 1 row, Excluded=true", rows)
	}
}

func TestEffectiveVersionExcludesUndefinedWorkflowDigest(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("%032d", 0), "tutor", "", "", runStatusCompleted, fixtureStart)

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	rows, err := db.effectiveVersionRowsForGaggle(context.Background(), "", "tutor", time.Time{})
	if err != nil {
		t.Fatalf("effectiveVersionRows: %v", err)
	}
	if len(rows) != 1 || !rows[0].Excluded {
		t.Fatalf("rows = %+v, want exactly 1 row, Excluded=true (no workflow_digest)", rows)
	}
}

func TestAssessEfficacyByEffectiveVersionPartialOverlapNotPooled(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	// Same prompt (workflow+goober digest), model changes: sonnet fails a
	// lot, opus is clean. If EffectiveVersion pooled on workflow_digest
	// alone (like the legacy AssessEfficacy), before/after would look
	// identical (same digest both sides) and never render a verdict at all.
	for i := 0; i < 5; i++ {
		seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("b%031d", i), "tutor", "sha256:aaaa", "sha256:goob1", runStatusFailed, base.Add(time.Duration(i)*time.Hour),
			agentSpanFixture{model: "claude-sonnet-5", harness: "1.0.0"})
	}
	for i := 0; i < 5; i++ {
		seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("a%031d", i), "tutor", "sha256:aaaa", "sha256:goob1", runStatusCompleted, base.Add(time.Duration(10+i)*time.Hour),
			agentSpanFixture{model: "claude-opus-5", harness: "1.0.0"})
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	result, err := db.AssessLatestEfficacyByEffectiveVersion(context.Background(), "tutor", time.Time{}, DefaultEfficacyThresholds())
	if err != nil {
		t.Fatalf("AssessLatestEfficacyByEffectiveVersion: %v", err)
	}
	if result.Verdict != EfficacyHelped {
		t.Fatalf("Verdict = %q, want helped: %+v", result.Verdict, result)
	}
	if result.OldVersion.Model != "claude-sonnet-5" || result.NewVersion.Model != "claude-opus-5" {
		t.Errorf("compared %+v -> %+v, want sonnet -> opus", result.OldVersion, result.NewVersion)
	}
	if result.Before.FailedRuns != 5 || result.After.CompletedRuns != 5 {
		t.Errorf("Before/After = %+v / %+v", result.Before, result.After)
	}
}

func TestAssessEfficacyByEffectiveVersionExcludesMixedRunsFromComparison(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	base := fixtureStart

	for i := 0; i < 5; i++ {
		seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("b%031d", i), "tutor", "sha256:aaaa", "sha256:goob1", runStatusCompleted, base.Add(time.Duration(i)*time.Hour),
			agentSpanFixture{model: "claude-sonnet-5", harness: "1.0.0"})
	}
	// A mixed run sits chronologically between the two cohorts. It must not
	// be counted in EITHER segment's stats, and must not itself register as
	// a distinct EffectiveVersion transition.
	seedEffectiveVersionRun(t, runsDir, "mixed0000000000000000000000000000", "tutor", "sha256:aaaa", "sha256:goob1", runStatusFailed, base.Add(5*time.Hour),
		agentSpanFixture{model: "claude-sonnet-5", harness: "1.0.0"},
		agentSpanFixture{model: "claude-opus-5", harness: "1.0.0"})
	for i := 0; i < 5; i++ {
		seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("a%031d", i), "tutor", "sha256:aaaa", "sha256:goob1", runStatusCompleted, base.Add(time.Duration(10+i)*time.Hour),
			agentSpanFixture{model: "claude-opus-5", harness: "1.0.0"})
	}

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	changes, err := db.DigestHistoryByEffectiveVersion(context.Background(), "tutor")
	if err != nil {
		t.Fatalf("DigestHistoryByEffectiveVersion: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want exactly 1 transition (mixed run invisible)", changes)
	}

	before, err := db.runStatsByEffectiveVersionForGaggle("", "tutor", changes[0].FromHash, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("runStatsByEffectiveVersion (before): %v", err)
	}
	if before.TotalRuns != 5 {
		t.Errorf("before.TotalRuns = %d, want 5 (mixed run excluded)", before.TotalRuns)
	}
	after, err := db.runStatsByEffectiveVersionForGaggle("", "tutor", changes[0].ToHash, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("runStatsByEffectiveVersion (after): %v", err)
	}
	if after.TotalRuns != 5 {
		t.Errorf("after.TotalRuns = %d, want 5 (mixed run excluded)", after.TotalRuns)
	}
}

func TestAssessLatestEfficacyByEffectiveVersionInsufficientDataWithNoTransition(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("%032d", 0), "tutor", "sha256:aaaa", "sha256:goob1", runStatusCompleted, fixtureStart,
		agentSpanFixture{model: "claude-sonnet-5", harness: "1.0.0"})

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	result, err := db.AssessLatestEfficacyByEffectiveVersion(context.Background(), "tutor", time.Time{}, DefaultEfficacyThresholds())
	if err != nil {
		t.Fatalf("AssessLatestEfficacyByEffectiveVersion: %v", err)
	}
	if result.Verdict != EfficacyInsufficientData {
		t.Fatalf("Verdict = %q, want insufficient-data (no transition observed)", result.Verdict)
	}
	if result.OldVersionHash != "" || result.NewVersionHash != "" {
		t.Errorf("OldVersionHash/NewVersionHash = %q/%q, want both empty (no transition)", result.OldVersionHash, result.NewVersionHash)
	}
}

func TestEffectiveVersionNonAgenticRunIsWellDefined(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	seedEffectiveVersionRun(t, runsDir, fmt.Sprintf("%032d", 0), "tutor", "sha256:aaaa", "sha256:goob1", runStatusCompleted, fixtureStart)

	db := openTestDB(t, tmp)
	seedAndIngest(t, db, runsDir)

	rows, err := db.effectiveVersionRowsForGaggle(context.Background(), "", "tutor", time.Time{})
	if err != nil {
		t.Fatalf("effectiveVersionRows: %v", err)
	}
	if len(rows) != 1 || rows[0].Excluded {
		t.Fatalf("rows = %+v, want exactly 1 row, Excluded=false (non-agentic run is a well-defined cohort)", rows)
	}
	if rows[0].Version.Model != "" || rows[0].Version.HarnessVersion != "" {
		t.Errorf("Version = %+v, want empty Model/HarnessVersion for a non-agentic run", rows[0].Version)
	}
}
