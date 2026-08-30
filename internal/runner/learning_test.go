package runner

import (
	"encoding/json"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
)

func TestRecordLearningInjectionPersistsCompleteEpisodeAndResumePointer(t *testing.T) {
	const runID = "learning-injection"
	runsDir := filepath.Join(t.TempDir(), "runs")
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "fixture", WorkflowVersion: 1,
		WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goober",
		Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()
	if err := jr.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review",
		Verdict: string(apiv1.VerdictNeedsChanges), Target: "implement",
		Runner: map[string]any{"repassAttempt": 1},
	}); err != nil {
		t.Fatal(err)
	}
	finding := apiv1.Finding{
		ID: "finding-1", LearningSignature: "review|validation|missing-test",
		LearningClassification: apiv1.LearningValidation,
		EvidenceDigest:         "sha256:evidence-1",
		Severity:               apiv1.SeverityError,
		Message:                "add the missing regression test",
		Location:               "internal/widget/widget_test.go",
	}
	pointer, err := recordLearningInjection(
		jr,
		StartInput{RunID: runID, Machine: fixtureMachine(t), Gaggle: "acme-web", GooberDigest: "sha256:goober"},
		"review", "implement",
		gate.Result{
			Attempt: 1,
			Verdict: &apiv1.Verdict{
				Decision:  apiv1.VerdictNeedsChanges,
				Rationale: "The regression remains uncovered.",
				Findings:  []apiv1.Finding{finding},
			},
		},
		"implement",
		apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if pointer == nil || pointer.Artifact == nil || pointer.Name != "learning.episode[2]" {
		t.Fatalf("episode pointer = %+v", pointer)
	}
	data, err := pointer.Artifact.Resolve(jr.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var episode learning.Episode
	if err := json.Unmarshal(data, &episode); err != nil {
		t.Fatal(err)
	}
	if episode.Schema != learning.EpisodeSchema ||
		episode.SourceRunID != runID || episode.SourceSeq != 2 ||
		episode.SourceAttempt != 1 || episode.NextAttempt != 2 ||
		episode.WorkflowDigest == "" || episode.GooberDigest != "sha256:goober" ||
		episode.EffectiveVersion == "" ||
		episode.RecommendedAction != learning.ActionTargetedTest ||
		episode.CorrectionFeedback != "The regression remains uncovered." ||
		episode.Outcome != learning.OutcomeUnresolved ||
		len(episode.Findings) != 1 || episode.Findings[0].ID != "finding-1" ||
		len(episode.Actions) != 1 || episode.Actions[0].FindingID != "finding-1" ||
		episode.Actions[0].RecommendedAction != learning.ActionTargetedTest {
		t.Fatalf("episode = %+v", episode)
	}

	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	pointers := reconstructPointers(events, fixtureMachine(t))
	if len(pointers) != 1 || pointers[0].Name != "learning.episode[2]" ||
		pointers[0].Artifact == nil || pointers[0].Artifact.Digest != pointer.Artifact.Digest {
		t.Fatalf("reconstructed pointers = %+v", pointers)
	}
}
