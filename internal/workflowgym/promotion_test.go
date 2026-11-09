package workflowgym

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestValidatePromotionEvidenceProducesAuditableReceipt(t *testing.T) {
	evidence, policy := validPromotionInputs()

	receipt, err := ValidatePromotionEvidence(evidence, policy)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Kind != PromotionValidationKind || receipt.Verdict != EvaluationPass ||
		receipt.EvidenceDigest == "" || receipt.PolicyDigest == "" ||
		!reflect.DeepEqual(receipt.AcceptedResults, []string{"benchmark", "shadow", "holdout"}) {
		t.Fatalf("receipt = %+v", receipt)
	}
	if receipt.Candidate != evidence.Candidate || receipt.ConfigDigest != evidence.ConfigDigest ||
		receipt.LastKnownGood != evidence.LastKnownGood || receipt.Digests != evidence.Digests {
		t.Fatalf("receipt lost promotion provenance: %+v", receipt)
	}
}

func TestValidatePromotionEvidenceRejectsMismatchedCandidateAndDataset(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PromotionEvidence)
		want   string
	}{
		{
			name: "policy candidate",
			mutate: func(e *PromotionEvidence) {
				e.Candidate = promotionIdentity("other")
				e.CandidateDigest = CandidateIdentityDigest(e.Candidate)
				e.Benchmark.CandidateDigest = e.CandidateDigest
				e.Shadow.CandidateDigest = e.CandidateDigest
				e.Holdout.CandidateDigest = e.CandidateDigest
			},
			want: "policy candidate",
		},
		{
			name: "result candidate",
			mutate: func(e *PromotionEvidence) {
				e.Shadow.CandidateDigest = CandidateIdentityDigest(promotionIdentity("other"))
			},
			want: "wrong candidate",
		},
		{
			name: "policy dataset",
			mutate: func(e *PromotionEvidence) {
				e.DatasetVersion = "dataset-other"
				e.Digests.Dataset = "sha256:dataset-other"
				e.Benchmark.DatasetVersion, e.Benchmark.DatasetDigest = e.DatasetVersion, e.Digests.Dataset
				e.Shadow.DatasetVersion, e.Shadow.DatasetDigest = e.DatasetVersion, e.Digests.Dataset
				e.Holdout.DatasetVersion, e.Holdout.DatasetDigest = e.DatasetVersion, e.Digests.Dataset
			},
			want: "policy dataset",
		},
		{
			name: "result dataset",
			mutate: func(e *PromotionEvidence) {
				e.Holdout.DatasetDigest = "sha256:dataset-other"
			},
			want: "wrong dataset",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence, policy := validPromotionInputs()
			test.mutate(&evidence)
			if _, err := ValidatePromotionEvidence(evidence, policy); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidatePromotionEvidenceRejectsStaleManifests(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PromotionEvidence)
		want   string
	}{
		{
			name:   "evaluator",
			mutate: func(e *PromotionEvidence) { e.Shadow.EvaluatorDigest = "sha256:stale" },
			want:   "stale evaluator manifest",
		},
		{
			name:   "environment",
			mutate: func(e *PromotionEvidence) { e.Holdout.EnvironmentDigest = "sha256:stale" },
			want:   "stale environment manifest",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence, policy := validPromotionInputs()
			test.mutate(&evidence)
			if _, err := ValidatePromotionEvidence(evidence, policy); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidatePromotionEvidenceRequiresPolicyEvidence(t *testing.T) {
	evidence, policy := validPromotionInputs()
	evidence.Shadow = nil
	evidence.Holdout = nil
	if _, err := ValidatePromotionEvidence(evidence, policy); err == nil || !strings.Contains(err.Error(), "required shadow evidence is missing") {
		t.Fatalf("error = %v", err)
	}

	evidence, policy = validPromotionInputs()
	policy.RequireShadow = false
	evidence.Shadow = nil
	evidence.Holdout = nil
	if _, err := ValidatePromotionEvidence(evidence, policy); err == nil || !strings.Contains(err.Error(), "required holdout evidence is missing") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidatePromotionEvidenceRejectsRegressedOrIncompleteResults(t *testing.T) {
	evidence, policy := validPromotionInputs()
	evidence.Shadow.Verdict = EvaluationFail
	if _, err := ValidatePromotionEvidence(evidence, policy); err == nil || !strings.Contains(err.Error(), `verdict is "fail"`) {
		t.Fatalf("error = %v", err)
	}

	evidence, policy = validPromotionInputs()
	evidence.Holdout.Samples = 0
	if _, err := ValidatePromotionEvidence(evidence, policy); err == nil || !strings.Contains(err.Error(), "no measured samples") {
		t.Fatalf("error = %v", err)
	}
}

func validPromotionInputs() (PromotionEvidence, PromotionPolicy) {
	candidate := promotionIdentity("candidate")
	lkg := promotionIdentity("lkg")
	digests := EvaluationDigests{
		Dataset:     "sha256:dataset",
		Evaluator:   "sha256:evaluator",
		Environment: "sha256:environment",
	}
	result := PromotionResult{
		ArtifactDigest:    "sha256:result",
		CandidateDigest:   CandidateIdentityDigest(candidate),
		DatasetVersion:    "dataset-1",
		DatasetDigest:     digests.Dataset,
		EvaluatorDigest:   digests.Evaluator,
		EnvironmentDigest: digests.Environment,
		Verdict:           EvaluationPass,
		Samples:           20,
	}
	shadow, holdout := result, result
	shadow.ArtifactDigest = "sha256:shadow"
	holdout.ArtifactDigest = "sha256:holdout"
	evidence := PromotionEvidence{
		APIVersion:      apiv1.WorkflowGymTaskAPIVersion,
		Kind:            PromotionEvidenceKind,
		Candidate:       candidate,
		CandidateDigest: CandidateIdentityDigest(candidate),
		ConfigDigest:    "sha256:config",
		LastKnownGood:   lkg,
		DatasetVersion:  "dataset-1",
		Digests:         digests,
		Benchmark:       result,
		Shadow:          &shadow,
		Holdout:         &holdout,
	}
	policy := PromotionPolicy{
		APIVersion:     apiv1.WorkflowGymTaskAPIVersion,
		Kind:           PromotionPolicyKind,
		Candidate:      candidate,
		ConfigDigest:   evidence.ConfigDigest,
		LastKnownGood:  lkg,
		DatasetVersion: evidence.DatasetVersion,
		Digests:        digests,
		RequireShadow:  true,
		RequireHoldout: true,
	}
	return evidence, policy
}

func promotionIdentity(name string) TournamentCandidateIdentity {
	return TournamentCandidateIdentity{
		EffectiveVersion: rollup.EffectiveVersion{
			WorkflowDigest: "sha256:workflow-" + name,
			GooberDigest:   "sha256:goober-" + name,
			Model:          "model-" + name,
			HarnessVersion: "harness-1",
		},
		GenomeDigest: "sha256:genome-" + name,
	}
}
