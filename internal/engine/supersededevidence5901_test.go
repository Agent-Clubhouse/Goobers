package engine

import (
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// TestGateEvidenceWithholdsSupersededValidationFromReviewer is the durable
// engine's half of #5901: the reviewer context collectGateEvidence hands the
// agentic gate drops a local-ci failure the agentic subject has since
// remediated, through the same runner rule the local runner applies.
func TestGateEvidenceWithholdsSupersededValidationFromReviewer(t *testing.T) {
	spec := gatedSpec()
	spec.Tasks[0].Next = "review"
	spec.Gates[0].Branches["pass"] = "local-ci"
	spec.Tasks = append(spec.Tasks, apiv1.Task{Name: "local-ci", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"make", "ci"}}, Next: "local-gate"})
	spec.Gates = append(spec.Gates, apiv1.Gate{
		Name:      "local-gate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "failure-class"},
		Branches:  map[string]string{"pass": wf.TerminalComplete, "fail": "implement", "infra": "local-ci"},
	})
	machine, err := wf.Compile(wf.Definition{Name: "stale-ci", Version: 1, Spec: spec},
		wf.WithKnownChecks([]string{"failure-class"}), wf.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rec := &runJournal{}
	now := time.Now()
	for _, ev := range []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Artifacts: []journal.Ref{{Path: "artifacts/impl-1"}}},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: gate.OutcomePass},
		{Type: journal.EventStageFinished, Stage: "local-ci", Artifacts: []journal.Ref{{Path: "artifacts/ci-fail"}}},
		{Type: journal.EventGateEvaluated, Gate: "local-gate", Verdict: gate.OutcomeFail},
		{Type: journal.EventStageFinished, Stage: "implement", Artifacts: []journal.Ref{{Path: "artifacts/impl-2"}}},
	} {
		rec.appendAt(now, ev)
	}
	pointers := []apiv1.ContextPointer{
		{Name: "local-ci.artifact[0]", Artifact: &apiv1.ArtifactPointer{Path: "artifacts/ci-fail"}},
		{Name: "implement.artifact[0]", Artifact: &apiv1.ArtifactPointer{Path: "artifacts/impl-2"}},
	}
	// The addendum skips the diff/repass-cause half, which needs a workflow
	// context; reviewer pointers are resolved before it either way.
	ev, err := collectGateEvidence(nil, machine, spec.Gates[0], "implement",
		apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, pointers, "rejected: read your inputs", rec)
	if err != nil {
		t.Fatalf("collectGateEvidence: %v", err)
	}
	if len(ev.ReviewerPointers) != 1 || ev.ReviewerPointers[0].Name != "implement.artifact[0]" {
		t.Fatalf("reviewer pointers = %+v, want only implement.artifact[0]", ev.ReviewerPointers)
	}
}
