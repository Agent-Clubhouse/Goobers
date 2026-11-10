package workflowgym

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestValidatePromotionEvidenceProducesAuditableReceipt(t *testing.T) {
	evidence, policy, reports := validPromotionInputs()

	receipt := mustValidatePromotion(t, evidence, policy, reports)
	if receipt.Kind != PromotionValidationKind || receipt.Verdict != EvaluationPass || len(receipt.Reasons) != 0 ||
		receipt.EvidenceDigest == "" || receipt.PolicyDigest == "" || len(receipt.Results) != 3 {
		t.Fatalf("receipt = %+v", receipt)
	}
	for i, stage := range []PromotionStage{PromotionStageBenchmark, PromotionStageShadow, PromotionStageHoldout} {
		got := receipt.Results[i]
		if got.Stage != stage || got.Verdict != EvaluationPass || got.Samples != 20 ||
			got.ArtifactDigest != EvaluationReportDigest(reports[stage]) {
			t.Fatalf("result %d = %+v", i, got)
		}
	}
	if receipt.Candidate != evidence.Candidate || receipt.ConfigDigest != evidence.ConfigDigest ||
		receipt.LastKnownGood != evidence.LastKnownGood || receipt.Digests != evidence.Digests ||
		receipt.LastKnownGoodDigest != CandidateIdentityDigest(evidence.LastKnownGood) {
		t.Fatalf("receipt lost promotion provenance: %+v", receipt)
	}
}

func TestValidatePromotionEvidenceRejectsMismatchedEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PromotionEvidence, *PromotionPolicy, map[PromotionStage]EvaluationReport)
		want   string
	}{
		{
			name: "policy candidate",
			mutate: func(_ *PromotionEvidence, p *PromotionPolicy, _ map[PromotionStage]EvaluationReport) {
				p.Candidate = promotionIdentity("other")
			},
			want: "does not match policy candidate",
		},
		{
			name: "policy dataset",
			mutate: func(_ *PromotionEvidence, p *PromotionPolicy, _ map[PromotionStage]EvaluationReport) {
				p.DatasetVersion = "dataset-other"
			},
			want: "does not match policy dataset",
		},
		{
			name: "stale evaluator",
			mutate: func(_ *PromotionEvidence, p *PromotionPolicy, _ map[PromotionStage]EvaluationReport) {
				p.Digests.Evaluator = "sha256:evaluator-new"
			},
			want: "stale evaluator manifest",
		},
		{
			name: "report dataset",
			mutate: func(e *PromotionEvidence, _ *PromotionPolicy, r map[PromotionStage]EvaluationReport) {
				rebindReport(e.Holdout, r, func(report *EvaluationReport) { report.Digests.Dataset = "sha256:dataset-other" })
			},
			want: "holdout report is for the wrong dataset",
		},
		{
			name: "report environment",
			mutate: func(e *PromotionEvidence, _ *PromotionPolicy, r map[PromotionStage]EvaluationReport) {
				rebindReport(e.Shadow, r, func(report *EvaluationReport) { report.Digests.Environment = "sha256:stale" })
			},
			want: "shadow report uses a stale environment manifest",
		},
		{
			name: "report candidate",
			mutate: func(e *PromotionEvidence, _ *PromotionPolicy, r map[PromotionStage]EvaluationReport) {
				rebindReport(e.Shadow, r, func(report *EvaluationReport) {
					report.Candidates[0].Candidate = promotionIdentity("other")
					report.Candidates[0].CandidateDigest = CandidateIdentityDigest(report.Candidates[0].Candidate)
				})
			},
			want: "shadow report does not evaluate the evidence candidate",
		},
		{
			name: "baseline is not lkg",
			mutate: func(e *PromotionEvidence, _ *PromotionPolicy, r map[PromotionStage]EvaluationReport) {
				rebindReport(&e.Benchmark, r, func(report *EvaluationReport) {
					report.Baseline.Authoritative = promotionIdentity("older")
					report.Baseline.AuthoritativeDigest = CandidateIdentityDigest(report.Baseline.Authoritative)
				})
			},
			want: "benchmark report baseline is not the last-known-good",
		},
		{
			name: "tampered report",
			mutate: func(_ *PromotionEvidence, _ *PromotionPolicy, r map[PromotionStage]EvaluationReport) {
				report := r[PromotionStageShadow]
				report.Candidates[0].Gate = EvaluationGate{Verdict: EvaluationPass, Reasons: []string{}}
				report.Baseline.PassRate = 0.1
				r[PromotionStageShadow] = report
			},
			want: "does not match artifactDigest",
		},
		{
			name: "regressed shadow",
			mutate: func(e *PromotionEvidence, _ *PromotionPolicy, r map[PromotionStage]EvaluationReport) {
				rebindReport(e.Shadow, r, func(report *EvaluationReport) {
					report.Candidates[0].Gate = EvaluationGate{Verdict: EvaluationFail, Reasons: []string{"train: regressed"}}
				})
			},
			want: `shadow evidence verdict is "fail": train: regressed`,
		},
		{
			name: "regressed holdout slice",
			mutate: func(e *PromotionEvidence, _ *PromotionPolicy, r map[PromotionStage]EvaluationReport) {
				rebindReport(e.Holdout, r, func(report *EvaluationReport) { report.Candidates[0].Slices[1].Delta = -0.2 })
			},
			want: `holdout evidence verdict is "fail"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence, policy, reports := validPromotionInputs()
			test.mutate(&evidence, &policy, reports)
			assertPromotionRejected(t, mustValidatePromotion(t, evidence, policy, reports), EvaluationFail, test.want)
		})
	}
}

func TestValidatePromotionEvidenceRejectsBenchmarkCopiedIntoOtherSlots(t *testing.T) {
	evidence, policy, reports := validPromotionInputs()
	shadow, holdout := evidence.Benchmark, evidence.Benchmark
	evidence.Shadow, evidence.Holdout = &shadow, &holdout
	reports[PromotionStageShadow] = reports[PromotionStageBenchmark]
	reports[PromotionStageHoldout] = reports[PromotionStageBenchmark]
	receipt := mustValidatePromotion(t, evidence, policy, reports)
	assertPromotionRejected(t, receipt, EvaluationFail, `shadow evidence declares stage "benchmark"`)
	assertPromotionRejected(t, receipt, EvaluationFail, `holdout evidence declares stage "benchmark"`)

	shadow.Stage, holdout.Stage = PromotionStageShadow, PromotionStageHoldout
	receipt = mustValidatePromotion(t, evidence, policy, reports)
	assertPromotionRejected(t, receipt, EvaluationFail, "shadow evidence reuses the benchmark artifact")
	assertPromotionRejected(t, receipt, EvaluationFail, "holdout evidence reuses the benchmark artifact")
}

func TestValidatePromotionEvidenceIsInconclusiveWithoutEnoughEvidence(t *testing.T) {
	evidence, policy, reports := validPromotionInputs()
	evidence.Shadow, evidence.Holdout = nil, nil
	receipt := mustValidatePromotion(t, evidence, policy, reports)
	assertPromotionRejected(t, receipt, EvaluationInconclusive, "required shadow evidence is missing")
	assertPromotionRejected(t, receipt, EvaluationInconclusive, "required holdout evidence is missing")

	evidence, policy, reports = validPromotionInputs()
	rebindReport(&evidence.Benchmark, reports, func(report *EvaluationReport) { report.Candidates[0].Slices[0].Samples = 5 })
	assertPromotionRejected(t, mustValidatePromotion(t, evidence, policy, reports), EvaluationInconclusive,
		"train: insufficient evidence: 5 measured samples, require 10")
}

func TestValidatePromotionEvidenceRejectsReportGatedWithLooserConfig(t *testing.T) {
	evidence, policy, reports := validPromotionInputs()
	loosen := func(report *EvaluationReport) {
		report.Config.RegressionThreshold = 0.99
		report.Candidates[0].Slices[0].Delta = -0.3
		report.Candidates[0].Slices[0].ConfidenceInterval = EvaluationInterval{Lower: -0.4, Upper: -0.2}
		report.Candidates[0].Slices[1].Delta = -0.3
		report.Candidates[0].Slices[1].ConfidenceInterval = EvaluationInterval{Lower: -0.4, Upper: -0.2}
	}
	rebindReport(&evidence.Benchmark, reports, loosen)
	rebindReport(evidence.Holdout, reports, loosen)
	receipt := mustValidatePromotion(t, evidence, policy, reports)
	assertPromotionRejected(t, receipt, EvaluationFail, "benchmark report gate config")
	assertPromotionRejected(t, receipt, EvaluationFail, "holdout report gate config")

	// Even a report whose config matches policy is re-gated rather than
	// trusted: a recorded pass over a regressed holdout slice still fails.
	evidence, policy, reports = validPromotionInputs()
	rebindReport(evidence.Holdout, reports, func(report *EvaluationReport) {
		report.Candidates[0].Slices[1].Delta = -0.3
		report.Candidates[0].Slices[1].Verdict = EvaluationPass
	})
	assertPromotionRejected(t, mustValidatePromotion(t, evidence, policy, reports), EvaluationFail,
		"holdout: pass-rate delta -0.3 meets regression threshold -0.05")
}

func TestValidatePromotionEvidenceRejectsStagesFromOneTournament(t *testing.T) {
	evidence, policy, reports := validPromotionInputs()
	benchmark := reports[PromotionStageBenchmark]
	for i, result := range []*PromotionResult{evidence.Shadow, evidence.Holdout} {
		report := benchmark
		report.Config.MinSamples += i + 1
		reports[result.Stage] = report
		result.ArtifactDigest = EvaluationReportDigest(report)
	}
	receipt := mustValidatePromotion(t, evidence, policy, reports)
	assertPromotionRejected(t, receipt, EvaluationFail, "shadow report is for the wrong dataset")
	assertPromotionRejected(t, receipt, EvaluationFail, "shadow report uses a stale environment manifest")
	assertPromotionRejected(t, receipt, EvaluationFail, "holdout report is for the wrong dataset")
}

func TestValidatePromotionEvidenceRejectsMalformedInput(t *testing.T) {
	for name, mutate := range map[string]func(*PromotionPolicy){
		"minSamples":                         func(p *PromotionPolicy) { p.Evaluation.MinSamples = 0 },
		"shadowDigests is required":          func(p *PromotionPolicy) { p.ShadowDigests = nil },
		"holdoutDigests is required":         func(p *PromotionPolicy) { p.HoldoutDigests = nil },
		"shadowDigests.environment must":     func(p *PromotionPolicy) { p.ShadowDigests.Environment = p.Digests.Environment },
		"holdoutDigests.dataset must differ": func(p *PromotionPolicy) { p.HoldoutDigests.Dataset = p.Digests.Dataset },
	} {
		evidence, policy, reports := validPromotionInputs()
		mutate(&policy)
		if _, err := ValidatePromotionEvidence(evidence, policy, reports); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
	evidence, policy, reports := validPromotionInputs()
	evidence.Shadow.Report = ""
	if _, err := ValidatePromotionEvidence(evidence, policy, reports); err == nil || !strings.Contains(err.Error(), "shadow evidence report is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidatePromotionFilesLoadsReportsRelativeToEvidence(t *testing.T) {
	evidence, policy, reports := validPromotionInputs()
	dir := t.TempDir()
	for stage, report := range reports {
		writePromotionJSON(t, filepath.Join(dir, "reports", string(stage)+".json"), report)
	}
	evidencePath := filepath.Join(dir, "evidence.json")
	writePromotionJSON(t, evidencePath, evidence)
	policyPath := filepath.Join(dir, "policy.json")
	writePromotionJSON(t, policyPath, policy)

	receipt, err := ValidatePromotionFiles(evidencePath, policyPath)
	if err != nil || receipt.Verdict != EvaluationPass {
		t.Fatalf("receipt = %+v, err = %v", receipt, err)
	}
	if err := os.Remove(filepath.Join(dir, "reports", "holdout.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePromotionFiles(evidencePath, policyPath); err == nil || !strings.Contains(err.Error(), "load holdout evaluation report") {
		t.Fatalf("error = %v", err)
	}
}

func mustValidatePromotion(t *testing.T, evidence PromotionEvidence, policy PromotionPolicy, reports map[PromotionStage]EvaluationReport) PromotionValidation {
	t.Helper()
	receipt, err := ValidatePromotionEvidence(evidence, policy, reports)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func assertPromotionRejected(t *testing.T, receipt PromotionValidation, verdict EvaluationVerdict, want string) {
	t.Helper()
	if receipt.Verdict != verdict {
		t.Fatalf("verdict = %q, want %q; reasons = %q", receipt.Verdict, verdict, receipt.Reasons)
	}
	for _, reason := range receipt.Reasons {
		if strings.Contains(reason, want) {
			return
		}
	}
	t.Fatalf("reasons = %q, want %q", receipt.Reasons, want)
}

// rebindReport mutates a stage's report and re-digests it, modelling a
// genuine but unsuitable report rather than a tampered one.
func rebindReport(result *PromotionResult, reports map[PromotionStage]EvaluationReport, mutate func(*EvaluationReport)) {
	report := reports[result.Stage]
	report.Candidates = append([]EvaluationCandidateReport{}, report.Candidates...)
	report.Candidates[0].Slices = append([]EvaluationSlice{}, report.Candidates[0].Slices...)
	mutate(&report)
	reports[result.Stage] = report
	result.ArtifactDigest = EvaluationReportDigest(report)
}

func writePromotionJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func validPromotionInputs() (PromotionEvidence, PromotionPolicy, map[PromotionStage]EvaluationReport) {
	candidate := promotionIdentity("candidate")
	lkg := promotionIdentity("lkg")
	digests := EvaluationDigests{
		Dataset:     "sha256:dataset",
		Evaluator:   "sha256:evaluator",
		Environment: "sha256:environment",
	}
	stageDigests := map[PromotionStage]EvaluationDigests{
		PromotionStageBenchmark: digests,
		PromotionStageShadow:    {Dataset: "sha256:dataset-shadow", Evaluator: digests.Evaluator, Environment: "sha256:environment-shadow"},
		PromotionStageHoldout:   {Dataset: "sha256:dataset-holdout", Evaluator: digests.Evaluator, Environment: digests.Environment},
	}
	shadowDigests, holdoutDigests := stageDigests[PromotionStageShadow], stageDigests[PromotionStageHoldout]
	reports := map[PromotionStage]EvaluationReport{}
	results := map[PromotionStage]PromotionResult{}
	for _, stage := range []PromotionStage{PromotionStageBenchmark, PromotionStageShadow, PromotionStageHoldout} {
		report := promotionReport(candidate, lkg, stageDigests[stage])
		reports[stage] = report
		results[stage] = PromotionResult{Stage: stage, Report: "reports/" + string(stage) + ".json", ArtifactDigest: EvaluationReportDigest(report)}
	}
	shadow, holdout := results[PromotionStageShadow], results[PromotionStageHoldout]
	evidence := PromotionEvidence{
		APIVersion:      apiv1.WorkflowGymTaskAPIVersion,
		Kind:            PromotionEvidenceKind,
		Candidate:       candidate,
		CandidateDigest: CandidateIdentityDigest(candidate),
		ConfigDigest:    "sha256:config",
		LastKnownGood:   lkg,
		DatasetVersion:  "dataset-1",
		Digests:         digests,
		Benchmark:       results[PromotionStageBenchmark],
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
		ShadowDigests:  &shadowDigests,
		HoldoutDigests: &holdoutDigests,
		Evaluation:     promotionEvaluationConfig(),
		RequireShadow:  true,
		RequireHoldout: true,
	}
	return evidence, policy, reports
}

func promotionEvaluationConfig() EvaluationConfig {
	return EvaluationConfig{MinSamples: DefaultEvaluationMinSamples, RegressionThreshold: DefaultRegressionThreshold}
}

func promotionReport(candidate, lkg TournamentCandidateIdentity, digests EvaluationDigests) EvaluationReport {
	interval := EvaluationInterval{Lower: -0.01, Upper: 0.01}
	return EvaluationReport{
		APIVersion:      apiv1.WorkflowGymTaskAPIVersion,
		Kind:            EvaluationReportKind,
		Config:          promotionEvaluationConfig(),
		ConfidenceLevel: EvaluationConfidenceLevel,
		Digests:         digests,
		Gate:            EvaluationGate{Verdict: EvaluationPass, Reasons: []string{}},
		Baseline: EvaluationBaseline{
			Authoritative: lkg, AuthoritativeDigest: CandidateIdentityDigest(lkg),
			Samples: 20, PassRate: 0.9,
		},
		Candidates: []EvaluationCandidateReport{{
			Candidate:       candidate,
			CandidateDigest: CandidateIdentityDigest(candidate),
			Gate:            EvaluationGate{Verdict: EvaluationPass, Reasons: []string{}},
			Slices: []EvaluationSlice{
				{Split: apiv1.WorkflowGymSplitTrain, Selection: true, Samples: 20, Verdict: EvaluationPass, ConfidenceInterval: interval},
				{Split: apiv1.WorkflowGymSplitHoldout, Samples: 20, ConfidenceInterval: interval},
			},
		}},
	}
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
