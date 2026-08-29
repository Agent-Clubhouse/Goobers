package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// writeEffectiveVersionFixtureRun hand-constructs a run journal pinned to
// workflowDigest/gooberDigest with a single agentic task span carrying
// model/harnessVersion — the TUT-P1/TUT-P2 provenance axes TUT-P3 folds into
// EffectiveVersion.
func writeEffectiveVersionFixtureRun(
	t *testing.T, root, runID, workflow, workflowDigest, gooberDigest, model, harnessVersion string, status journal.RunPhase,
) {
	writeEffectiveVersionFixtureRunForGaggle(
		t, root, "example", runID, workflow, workflowDigest, gooberDigest, model, harnessVersion, status,
	)
}

func writeEffectiveVersionFixtureRunForGaggle(
	t *testing.T, root, gaggle, runID, workflow, workflowDigest, gooberDigest, model, harnessVersion string, status journal.RunPhase,
) {
	t.Helper()
	l := instance.NewLayout(root)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        workflow,
		WorkflowVersion: 1,
		WorkflowDigest:  workflowDigest,
		GooberDigest:    gooberDigest,
		Gaggle:          gaggle,
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create effective-version fixture run: %v", err)
	}
	defer func() { _ = jr.Close() }()

	resultStatus := "success"
	if status == journal.PhaseFailed {
		resultStatus = "failure"
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: resultStatus}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(status)}); err != nil {
		t.Fatal(err)
	}

	span := telemetry.SpanRecord{
		Schema:    telemetry.SpanSchema,
		TraceID:   runID,
		SpanID:    "0123456789abcdef",
		Name:      "task/implement",
		Kind:      telemetry.SpanKindTask,
		StartTime: time.Now().UTC().Add(-time.Minute),
		EndTime:   time.Now().UTC().Add(time.Minute),
		Status:    "ok",
		Attributes: map[string]string{
			telemetry.AttrStage:          "implement",
			telemetry.AttrAttemptNumber:  "1",
			telemetry.AttrModel:          model,
			telemetry.AttrHarnessVersion: harnessVersion,
		},
	}
	spanData, err := json.Marshal(span)
	if err != nil {
		t.Fatal(err)
	}
	spanDir := filepath.Join(l.RunsDir(), runID, "spans")
	if err := os.MkdirAll(spanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spanDir, "spans.jsonl"), append(spanData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryQueryAggregateAcceptsCoverageGapKinds(t *testing.T) {
	for _, name := range []string{"workflow-untriggered", "stage-unreached", "ci-check-failure"} {
		t.Run(name, func(t *testing.T) {
			root := initDemo(t)
			code, _, stderr := runArgs(t, "telemetry-query", "--window", "24h", "--aggregate", name, root)
			if code != 0 {
				t.Fatalf("code = %d, want 0 (valid aggregate name); stderr = %q", code, stderr)
			}
		})
	}
}

func TestTelemetryQueryEffectiveVersionEfficacyRequiresWorkflow(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "telemetry-query", "--format", "effective-version-efficacy", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2 (usage error); stderr = %q", code, stderr)
	}
}

func TestTelemetryQueryEffectiveVersionEfficacyNoTransitionIsNoWork(t *testing.T) {
	root := initDemo(t)
	writeEffectiveVersionFixtureRun(t, root, "ev-run-1", "tutor", "sha256:aaaa", "sha256:goob1", "claude-sonnet-5", "1.0.0", journal.PhaseCompleted)
	rebuildTelemetryQueryRollup(t, root)

	code, stdout, stderr := runArgs(t, "telemetry-query", "--format", "effective-version-efficacy", "--workflow", "tutor", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got effectiveVersionEfficacyArtifact
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not parseable JSON: %v\n%s", err, stdout)
	}
	if !got.NoWork || got.Note != telemetryQueryNoEffectiveVersionChangeNote {
		t.Fatalf("got = %+v, want NoWork=true with the no-transition note", got)
	}
	if got.Verdict != rollup.EfficacyInsufficientData {
		t.Fatalf("verdict = %q, want insufficient-data", got.Verdict)
	}
}

func TestTelemetryQueryEffectiveVersionEfficacyDetectsModelOnlyTransition(t *testing.T) {
	root := initDemo(t)
	// Same workflow+goober digest, model changes sonnet -> opus: a partial-
	// overlap cohort boundary the legacy workflow_digest-only view would
	// never see (it would report zero transitions and no verdict).
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "ev-before-"+string(rune('a'+i)), "tutor", "sha256:aaaa", "sha256:goob1", "claude-sonnet-5", "1.0.0", journal.PhaseFailed)
	}

	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "ev-after-"+string(rune('a'+i)), "tutor", "sha256:aaaa", "sha256:goob1", "claude-opus-5", "1.0.0", journal.PhaseCompleted)
	}
	rebuildTelemetryQueryRollup(t, root)

	code, stdout, stderr := runArgs(t,
		"telemetry-query", "--format", "effective-version-efficacy", "--workflow", "tutor",
		"--threshold", "min-samples=5", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got effectiveVersionEfficacyArtifact
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not parseable JSON: %v\n%s", err, stdout)
	}
	if got.NoWork {
		t.Fatalf("got = %+v, want a real transition, not no-work", got)
	}
	if got.Verdict != rollup.EfficacyHelped {
		t.Fatalf("verdict = %q, want helped: %+v", got.Verdict, got)
	}
	if got.OldVersion == nil || got.NewVersion == nil {
		t.Fatalf("OldVersion/NewVersion = %+v/%+v, want both set", got.OldVersion, got.NewVersion)
	}
	if got.OldVersion.Model != "claude-sonnet-5" || got.NewVersion.Model != "claude-opus-5" {
		t.Fatalf("compared %+v -> %+v, want sonnet -> opus", got.OldVersion, got.NewVersion)
	}
}

func TestTelemetryQueryEffectiveVersionEfficacyScopesToGaggle(t *testing.T) {
	root := initDemo(t)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRunForGaggle(t, root, "alpha", "alpha-before-"+string(rune('a'+i)), "tutor", "sha256:aaaa", "sha256:goob1", "model-a", "1.0.0", journal.PhaseFailed)
		writeEffectiveVersionFixtureRunForGaggle(t, root, "alpha", "alpha-after-"+string(rune('a'+i)), "tutor", "sha256:bbbb", "sha256:goob2", "model-a", "1.0.0", journal.PhaseCompleted)
		writeEffectiveVersionFixtureRunForGaggle(t, root, "bravo", "bravo-before-"+string(rune('a'+i)), "tutor", "sha256:aaaa", "sha256:goob1", "model-a", "1.0.0", journal.PhaseCompleted)
		writeEffectiveVersionFixtureRunForGaggle(t, root, "bravo", "bravo-after-"+string(rune('a'+i)), "tutor", "sha256:bbbb", "sha256:goob2", "model-a", "1.0.0", journal.PhaseFailed)
	}
	rebuildTelemetryQueryRollup(t, root)

	code, stdout, stderr := runArgs(t,
		"telemetry-query", "--format", "effective-version-efficacy",
		"--gaggle", "alpha", "--workflow", "tutor",
		"--threshold", "min-samples=5", root,
	)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got effectiveVersionEfficacyArtifact
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not parseable JSON: %v\n%s", err, stdout)
	}
	if got.Verdict != rollup.EfficacyHelped {
		t.Fatalf("verdict = %q, want helped from alpha-only cohorts: %+v", got.Verdict, got)
	}
}

func TestTelemetryQueryEffectiveVersionEfficacyPreservesNoChangeThreshold(t *testing.T) {
	root := initDemo(t)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "same-before-"+string(rune('a'+i)), "tutor", "sha256:aaaa", "sha256:goob1", "model-a", "1.0.0", journal.PhaseCompleted)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "same-after-"+string(rune('a'+i)), "tutor", "sha256:bbbb", "sha256:goob2", "model-a", "1.0.0", journal.PhaseCompleted)
	}
	rebuildTelemetryQueryRollup(t, root)

	code, stdout, stderr := runArgs(t,
		"telemetry-query", "--format", "effective-version-efficacy", "--workflow", "tutor",
		"--threshold", "min-samples=5", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got effectiveVersionEfficacyArtifact
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not parseable JSON: %v\n%s", err, stdout)
	}
	if got.Verdict != rollup.EfficacyNoChange {
		t.Fatalf("verdict = %q, want no-change: %+v", got.Verdict, got)
	}
}
