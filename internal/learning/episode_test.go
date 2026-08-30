package learning

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestNormalizeFindingPreservesStableIdentityAndRoutesGovernedAction(t *testing.T) {
	finding := apiv1.Finding{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingMissingTests,
		Message:  "Add coverage for branch 42",
		Location: "internal/widget/widget.go:42",
	}
	NormalizeFinding(&finding, "review", "sha256:evidence")
	if finding.ID == "" || finding.LearningSignature == "" {
		t.Fatalf("normalized finding lacks durable identity: %+v", finding)
	}
	if finding.LearningClassification != apiv1.LearningValidation {
		t.Fatalf("classification = %q, want validation", finding.LearningClassification)
	}
	if RecommendedAction(finding.LearningClassification) != ActionTargetedTest {
		t.Fatalf("action = %q, want targeted test", RecommendedAction(finding.LearningClassification))
	}

	same := apiv1.Finding{
		Severity: apiv1.SeverityError,
		Class:    apiv1.FindingMissingTests,
		Message:  "Add coverage for branch 99",
		Location: "internal/widget/widget.go:99",
	}
	NormalizeFinding(&same, "review", "sha256:new-evidence")
	if same.ID != finding.ID || same.LearningSignature != finding.LearningSignature {
		t.Fatalf("normalized identity drifted across volatile line/numeric text:\nfirst=%+v\nsecond=%+v", finding, same)
	}
}

func TestRecommendedActionNeverSelectsModelWeightMutation(t *testing.T) {
	tests := map[apiv1.LearningClassification]string{
		apiv1.LearningInstruction: ActionInstructionOrSkill,
		apiv1.LearningSkill:       ActionInstructionOrSkill,
		apiv1.LearningWorkflow:    ActionWorkflowOrGate,
		apiv1.LearningGate:        ActionWorkflowOrGate,
		apiv1.LearningValidation:  ActionTargetedTest,
		apiv1.LearningCodeDefect:  ActionCodeIssue,
	}
	for classification, want := range tests {
		if got := RecommendedAction(classification); got != want {
			t.Fatalf("RecommendedAction(%q) = %q, want %q", classification, got, want)
		}
	}
}
