package workflowgym

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/strictyaml"
)

const (
	// PromotionEvidenceKind identifies evidence offered to Workflow CD.
	PromotionEvidenceKind = "WorkflowGymPromotionEvidence"
	// PromotionPolicyKind identifies the current Workflow CD validation inputs.
	PromotionPolicyKind = "WorkflowGymPromotionPolicy"
	// PromotionValidationKind identifies an auditable CD handoff receipt.
	PromotionValidationKind = "WorkflowGymPromotionValidation"
)

// PromotionStage names one evidence slot in a promotion handoff.
type PromotionStage string

const (
	// PromotionStageBenchmark is the offline benchmark evaluation.
	PromotionStageBenchmark PromotionStage = "benchmark"
	// PromotionStageShadow is the shadow-traffic evaluation.
	PromotionStageShadow PromotionStage = "shadow"
	// PromotionStageHoldout is the held-out evaluation, gated on its holdout slice.
	PromotionStageHoldout PromotionStage = "holdout"
)

// PromotionEvidence joins every result needed to consider one candidate for
// promotion. Validation is read-only and does not promote the candidate.
type PromotionEvidence struct {
	APIVersion      string                      `json:"apiVersion" yaml:"apiVersion"`
	Kind            string                      `json:"kind" yaml:"kind"`
	Candidate       TournamentCandidateIdentity `json:"candidate" yaml:"candidate"`
	CandidateDigest string                      `json:"candidateDigest" yaml:"candidateDigest"`
	ConfigDigest    string                      `json:"configDigest" yaml:"configDigest"`
	LastKnownGood   TournamentCandidateIdentity `json:"lastKnownGood" yaml:"lastKnownGood"`
	DatasetVersion  string                      `json:"datasetVersion" yaml:"datasetVersion"`
	Digests         EvaluationDigests           `json:"digests" yaml:"digests"`
	Benchmark       PromotionResult             `json:"benchmark" yaml:"benchmark"`
	Shadow          *PromotionResult            `json:"shadow,omitempty" yaml:"shadow,omitempty"`
	Holdout         *PromotionResult            `json:"holdout,omitempty" yaml:"holdout,omitempty"`
}

// PromotionResult references one WorkflowGymEvaluation report. Report is a
// path, relative to the evidence file unless absolute, and ArtifactDigest
// must equal EvaluationReportDigest of that report. Verdicts, sample counts,
// and manifest digests are always derived from the report itself.
type PromotionResult struct {
	Stage          PromotionStage `json:"stage" yaml:"stage"`
	Report         string         `json:"report" yaml:"report"`
	ArtifactDigest string         `json:"artifactDigest" yaml:"artifactDigest"`
}

// PromotionPolicy is the Workflow CD state and evidence policy at validation
// time. Exact digest matching makes stale evidence fail closed.
type PromotionPolicy struct {
	APIVersion     string                      `json:"apiVersion" yaml:"apiVersion"`
	Kind           string                      `json:"kind" yaml:"kind"`
	Candidate      TournamentCandidateIdentity `json:"candidate" yaml:"candidate"`
	ConfigDigest   string                      `json:"configDigest" yaml:"configDigest"`
	LastKnownGood  TournamentCandidateIdentity `json:"lastKnownGood" yaml:"lastKnownGood"`
	DatasetVersion string                      `json:"datasetVersion" yaml:"datasetVersion"`
	Digests        EvaluationDigests           `json:"digests" yaml:"digests"`
	MinSamples     int                         `json:"minSamples" yaml:"minSamples"`
	RequireShadow  bool                        `json:"requireShadow" yaml:"requireShadow"`
	RequireHoldout bool                        `json:"requireHoldout" yaml:"requireHoldout"`
}

// PromotionValidation is the deterministic receipt Workflow CD can audit and
// consume. It is produced for accepted and rejected evidence alike; only a
// pass verdict permits promotion.
type PromotionValidation struct {
	APIVersion          string                      `json:"apiVersion"`
	Kind                string                      `json:"kind"`
	Verdict             EvaluationVerdict           `json:"verdict"`
	Reasons             []string                    `json:"reasons"`
	EvidenceDigest      string                      `json:"evidenceDigest"`
	PolicyDigest        string                      `json:"policyDigest"`
	Candidate           TournamentCandidateIdentity `json:"candidate"`
	CandidateDigest     string                      `json:"candidateDigest"`
	ConfigDigest        string                      `json:"configDigest"`
	LastKnownGood       TournamentCandidateIdentity `json:"lastKnownGood"`
	LastKnownGoodDigest string                      `json:"lastKnownGoodDigest"`
	DatasetVersion      string                      `json:"datasetVersion"`
	Digests             EvaluationDigests           `json:"digests"`
	Results             []PromotionResultReceipt    `json:"results"`
}

// PromotionResultReceipt records what one referenced report proved.
type PromotionResultReceipt struct {
	Stage          PromotionStage    `json:"stage"`
	ArtifactDigest string            `json:"artifactDigest"`
	Verdict        EvaluationVerdict `json:"verdict"`
	Samples        int               `json:"samples"`
}

// CandidateIdentityDigest returns the canonical identity used to bind result
// artifacts to a promotion candidate.
func CandidateIdentityDigest(identity TournamentCandidateIdentity) string {
	return digestJSON(identity)
}

// EvaluationReportDigest returns the canonical digest of an evaluation report:
// SHA-256 over its compact JSON encoding.
func EvaluationReportDigest(report EvaluationReport) string {
	return digestJSON(report)
}

// LoadPromotionEvidence strictly decodes a JSON or YAML promotion artifact.
func LoadPromotionEvidence(path string) (PromotionEvidence, error) {
	var evidence PromotionEvidence
	if err := loadPromotionDocument(path, &evidence); err != nil {
		return PromotionEvidence{}, fmt.Errorf("load promotion evidence %s: %w", path, err)
	}
	return evidence, nil
}

// LoadPromotionPolicy strictly decodes a JSON or YAML CD policy.
func LoadPromotionPolicy(path string) (PromotionPolicy, error) {
	var policy PromotionPolicy
	if err := loadPromotionDocument(path, &policy); err != nil {
		return PromotionPolicy{}, fmt.Errorf("load promotion policy %s: %w", path, err)
	}
	return policy, nil
}

// ValidatePromotionFiles loads evidence, policy, and every referenced
// evaluation report, then validates them. An error means an input could not be
// loaded or is malformed; rejections are reported in the returned receipt.
func ValidatePromotionFiles(evidencePath, policyPath string) (PromotionValidation, error) {
	evidence, err := LoadPromotionEvidence(evidencePath)
	if err != nil {
		return PromotionValidation{}, err
	}
	policy, err := LoadPromotionPolicy(policyPath)
	if err != nil {
		return PromotionValidation{}, err
	}
	reports := map[PromotionStage]EvaluationReport{}
	for _, slot := range promotionSlots(evidence) {
		if slot.result == nil || strings.TrimSpace(slot.result.Report) == "" {
			continue
		}
		path := slot.result.Report
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(evidencePath), path)
		}
		var report EvaluationReport
		if err := loadStrictJSON(path, &report); err != nil {
			return PromotionValidation{}, fmt.Errorf("load %s evaluation report %s: %w", slot.stage, path, err)
		}
		reports[slot.stage] = report
	}
	return ValidatePromotionEvidence(evidence, policy, reports)
}

// WritePromotionValidation writes a receipt as indented JSON.
func WritePromotionValidation(w io.Writer, validation PromotionValidation) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(validation); err != nil {
		return fmt.Errorf("write promotion validation: %w", err)
	}
	return nil
}

// ValidatePromotionEvidence validates a handoff against policy and the
// evaluation reports referenced by each evidence slot, without mutating
// Workflow CD or any production state. It returns an error only for
// malformed input; mismatched, regressed, or incomplete evidence yields a
// fail or inconclusive receipt.
func ValidatePromotionEvidence(evidence PromotionEvidence, policy PromotionPolicy, reports map[PromotionStage]EvaluationReport) (PromotionValidation, error) {
	if err := validatePromotionPolicy(policy); err != nil {
		return PromotionValidation{}, fmt.Errorf("workflow gym promotion: invalid policy: %w", err)
	}
	if err := validatePromotionEvidenceHeader(evidence); err != nil {
		return PromotionValidation{}, fmt.Errorf("workflow gym promotion: invalid evidence: %w", err)
	}
	validation := PromotionValidation{
		APIVersion:          apiv1.WorkflowGymTaskAPIVersion,
		Kind:                PromotionValidationKind,
		Verdict:             EvaluationPass,
		Reasons:             []string{},
		EvidenceDigest:      digestJSON(evidence),
		PolicyDigest:        digestJSON(policy),
		Candidate:           evidence.Candidate,
		CandidateDigest:     evidence.CandidateDigest,
		ConfigDigest:        evidence.ConfigDigest,
		LastKnownGood:       evidence.LastKnownGood,
		LastKnownGoodDigest: CandidateIdentityDigest(evidence.LastKnownGood),
		DatasetVersion:      evidence.DatasetVersion,
		Digests:             evidence.Digests,
		Results:             []PromotionResultReceipt{},
	}
	validation.checkPolicy(evidence, policy)
	seen := map[string]PromotionStage{}
	for _, slot := range promotionSlots(evidence) {
		if err := validation.checkSlot(slot, evidence, policy, reports, seen); err != nil {
			return PromotionValidation{}, err
		}
	}
	return validation, nil
}

func (v *PromotionValidation) reject(verdict EvaluationVerdict, format string, args ...any) {
	v.Verdict = worseEvaluationVerdict(v.Verdict, verdict)
	v.Reasons = append(v.Reasons, fmt.Sprintf(format, args...))
}

func (v *PromotionValidation) checkPolicy(evidence PromotionEvidence, policy PromotionPolicy) {
	if evidence.Candidate != policy.Candidate {
		v.reject(EvaluationFail, "evidence candidate does not match policy candidate")
	}
	if evidence.ConfigDigest != policy.ConfigDigest {
		v.reject(EvaluationFail, "evidence config digest does not match policy candidate")
	}
	if evidence.LastKnownGood != policy.LastKnownGood {
		v.reject(EvaluationFail, "evidence last-known-good does not match policy")
	}
	if evidence.DatasetVersion != policy.DatasetVersion || evidence.Digests.Dataset != policy.Digests.Dataset {
		v.reject(EvaluationFail, "evidence dataset does not match policy dataset")
	}
	if evidence.Digests.Evaluator != policy.Digests.Evaluator {
		v.reject(EvaluationFail, "stale evaluator manifest")
	}
	if evidence.Digests.Environment != policy.Digests.Environment {
		v.reject(EvaluationFail, "stale environment manifest")
	}
}

type promotionSlot struct {
	stage    PromotionStage
	result   *PromotionResult
	required func(PromotionPolicy) bool
}

func promotionSlots(evidence PromotionEvidence) []promotionSlot {
	return []promotionSlot{
		{stage: PromotionStageBenchmark, result: &evidence.Benchmark, required: func(PromotionPolicy) bool { return true }},
		{stage: PromotionStageShadow, result: evidence.Shadow, required: func(p PromotionPolicy) bool { return p.RequireShadow }},
		{stage: PromotionStageHoldout, result: evidence.Holdout, required: func(p PromotionPolicy) bool { return p.RequireHoldout }},
	}
}

func (v *PromotionValidation) checkSlot(slot promotionSlot, evidence PromotionEvidence, policy PromotionPolicy, reports map[PromotionStage]EvaluationReport, seen map[string]PromotionStage) error {
	if slot.result == nil {
		if slot.required(policy) {
			v.reject(EvaluationInconclusive, "required %s evidence is missing", slot.stage)
		}
		return nil
	}
	result := *slot.result
	if err := requirePromotionResultFields(slot.stage, result); err != nil {
		return err
	}
	report, ok := reports[slot.stage]
	if !ok {
		return fmt.Errorf("workflow gym promotion: %s evaluation report %s was not loaded", slot.stage, result.Report)
	}
	receipt := PromotionResultReceipt{Stage: slot.stage, ArtifactDigest: result.ArtifactDigest, Verdict: EvaluationFail}
	defer func() { v.Results = append(v.Results, receipt) }()
	if result.Stage != slot.stage {
		v.reject(EvaluationFail, "%s evidence declares stage %q", slot.stage, result.Stage)
		return nil
	}
	if previous, duplicate := seen[result.ArtifactDigest]; duplicate {
		v.reject(EvaluationFail, "%s evidence reuses the %s artifact %s", slot.stage, previous, result.ArtifactDigest)
		return nil
	}
	seen[result.ArtifactDigest] = slot.stage
	if digest := EvaluationReportDigest(report); digest != result.ArtifactDigest {
		v.reject(EvaluationFail, "%s report digest %s does not match artifactDigest %s", slot.stage, digest, result.ArtifactDigest)
		return nil
	}
	if !v.checkReportProvenance(slot.stage, report, evidence) {
		return nil
	}
	receipt.Verdict, receipt.Samples = v.checkReportOutcome(slot.stage, report, evidence, policy)
	return nil
}

func requirePromotionResultFields(stage PromotionStage, result PromotionResult) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"stage", string(result.Stage)},
		{"report", result.Report},
		{"artifactDigest", result.ArtifactDigest},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("workflow gym promotion: %s evidence %s is required", stage, field.name)
		}
	}
	return nil
}

// checkReportProvenance binds a report to the evidence header and the
// last-known-good baseline. It reports whether the report can be trusted.
func (v *PromotionValidation) checkReportProvenance(stage PromotionStage, report EvaluationReport, evidence PromotionEvidence) bool {
	before := len(v.Reasons)
	if report.APIVersion != apiv1.WorkflowGymTaskAPIVersion || report.Kind != EvaluationReportKind {
		v.reject(EvaluationFail, "%s report is not a %s", stage, EvaluationReportKind)
	}
	if report.Digests.Dataset != evidence.Digests.Dataset {
		v.reject(EvaluationFail, "%s report is for the wrong dataset", stage)
	}
	if report.Digests.Evaluator != evidence.Digests.Evaluator {
		v.reject(EvaluationFail, "%s report uses a stale evaluator manifest", stage)
	}
	if report.Digests.Environment != evidence.Digests.Environment {
		v.reject(EvaluationFail, "%s report uses a stale environment manifest", stage)
	}
	if report.Baseline.AuthoritativeDigest != CandidateIdentityDigest(evidence.LastKnownGood) ||
		report.Baseline.Authoritative != evidence.LastKnownGood {
		v.reject(EvaluationFail, "%s report baseline is not the last-known-good", stage)
	}
	return len(v.Reasons) == before
}

// checkReportOutcome derives the stage verdict and measured samples from the
// report's evaluation of the evidence candidate.
func (v *PromotionValidation) checkReportOutcome(stage PromotionStage, report EvaluationReport, evidence PromotionEvidence, policy PromotionPolicy) (EvaluationVerdict, int) {
	candidate, ok := findEvaluationCandidate(report, evidence)
	if !ok {
		v.reject(EvaluationFail, "%s report does not evaluate the evidence candidate", stage)
		return EvaluationFail, 0
	}
	verdict, samples, reasons := promotionStageOutcome(stage, candidate, report.Config)
	if samples < policy.MinSamples {
		verdict = worseEvaluationVerdict(verdict, EvaluationInconclusive)
		reasons = append(reasons, fmt.Sprintf("insufficient evidence: %d measured samples, policy requires %d", samples, policy.MinSamples))
	}
	for _, reason := range reasons {
		v.reject(verdict, "%s evidence verdict is %q: %s", stage, verdict, reason)
	}
	return verdict, samples
}

func findEvaluationCandidate(report EvaluationReport, evidence PromotionEvidence) (EvaluationCandidateReport, bool) {
	for _, candidate := range report.Candidates {
		if candidate.CandidateDigest == evidence.CandidateDigest && candidate.Candidate == evidence.Candidate {
			return candidate, true
		}
	}
	return EvaluationCandidateReport{}, false
}

// promotionStageOutcome uses the candidate gate for benchmark and shadow
// reports. Holdout slices never drive the slice-6 gate, so a holdout report is
// gated by re-applying the report's own slice verdict to its holdout slice.
func promotionStageOutcome(stage PromotionStage, candidate EvaluationCandidateReport, config EvaluationConfig) (EvaluationVerdict, int, []string) {
	if stage != PromotionStageHoldout {
		samples := 0
		for _, slice := range candidate.Slices {
			if slice.Selection {
				samples += slice.Samples
			}
		}
		verdict := candidate.Gate.Verdict
		reasons := append([]string{}, candidate.Gate.Reasons...)
		if verdict != EvaluationPass && len(reasons) == 0 {
			reasons = append(reasons, "candidate gate did not pass")
		}
		return verdict, samples, reasons
	}
	for _, slice := range candidate.Slices {
		if slice.Split != apiv1.WorkflowGymSplitHoldout {
			continue
		}
		counts := evaluationCounts{samples: slice.Samples, missing: slice.MissingResults}
		verdict, reason := evaluationSliceVerdict(counts, slice.Delta, slice.ConfidenceInterval, config)
		if reason == "" {
			return verdict, slice.Samples, nil
		}
		return verdict, slice.Samples, []string{reason}
	}
	return EvaluationInconclusive, 0, []string{"no holdout slice"}
}

func validatePromotionPolicy(policy PromotionPolicy) error {
	if policy.APIVersion != apiv1.WorkflowGymTaskAPIVersion || policy.Kind != PromotionPolicyKind {
		return errors.New("invalid apiVersion or kind")
	}
	if err := validateEvaluationIdentity(policy.Candidate); err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	if err := validateEvaluationIdentity(policy.LastKnownGood); err != nil {
		return fmt.Errorf("lastKnownGood: %w", err)
	}
	if policy.MinSamples < 1 {
		return errors.New("minSamples must be at least 1")
	}
	return requirePromotionFields(policy.ConfigDigest, policy.DatasetVersion, policy.Digests)
}

func validatePromotionEvidenceHeader(evidence PromotionEvidence) error {
	if evidence.APIVersion != apiv1.WorkflowGymTaskAPIVersion || evidence.Kind != PromotionEvidenceKind {
		return errors.New("invalid apiVersion or kind")
	}
	if err := validateEvaluationIdentity(evidence.Candidate); err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	if evidence.CandidateDigest != CandidateIdentityDigest(evidence.Candidate) {
		return errors.New("candidateDigest does not identify candidate")
	}
	if err := validateEvaluationIdentity(evidence.LastKnownGood); err != nil {
		return fmt.Errorf("lastKnownGood: %w", err)
	}
	return requirePromotionFields(evidence.ConfigDigest, evidence.DatasetVersion, evidence.Digests)
}

func requirePromotionFields(configDigest, datasetVersion string, digests EvaluationDigests) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"configDigest", configDigest},
		{"datasetVersion", datasetVersion},
		{"digests.dataset", digests.Dataset},
		{"digests.evaluator", digests.Evaluator},
		{"digests.environment", digests.Environment},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	return nil
}

func loadPromotionDocument(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	jsonData, err := strictyaml.YAMLToJSON(raw)
	if err != nil {
		return err
	}
	return decodeStrictJSON(jsonData, target)
}

// loadStrictJSON decodes JSON-only artifacts such as evaluation reports, so
// their canonical digest is computed over exactly the emitted values.
func loadStrictJSON(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrictJSON(raw, target)
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("document contains multiple values")
		}
		return err
	}
	return nil
}
