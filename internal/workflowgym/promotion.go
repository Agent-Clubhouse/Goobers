package workflowgym

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/strictyaml"
)

const (
	// PromotionEvidenceKind identifies evidence offered to Workflow CD.
	PromotionEvidenceKind = "WorkflowGymPromotionEvidence"
	// PromotionPolicyKind identifies the current Workflow CD validation inputs.
	PromotionPolicyKind = "WorkflowGymPromotionPolicy"
	// PromotionValidationKind identifies an accepted, auditable CD handoff.
	PromotionValidationKind = "WorkflowGymPromotionValidation"
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

// PromotionResult binds one result artifact to the candidate and manifests it
// measured.
type PromotionResult struct {
	ArtifactDigest    string            `json:"artifactDigest" yaml:"artifactDigest"`
	CandidateDigest   string            `json:"candidateDigest" yaml:"candidateDigest"`
	DatasetVersion    string            `json:"datasetVersion" yaml:"datasetVersion"`
	DatasetDigest     string            `json:"datasetDigest" yaml:"datasetDigest"`
	EvaluatorDigest   string            `json:"evaluatorDigest" yaml:"evaluatorDigest"`
	EnvironmentDigest string            `json:"environmentDigest" yaml:"environmentDigest"`
	Verdict           EvaluationVerdict `json:"verdict" yaml:"verdict"`
	Samples           int               `json:"samples" yaml:"samples"`
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
	RequireShadow  bool                        `json:"requireShadow" yaml:"requireShadow"`
	RequireHoldout bool                        `json:"requireHoldout" yaml:"requireHoldout"`
}

// PromotionValidation is the deterministic receipt Workflow CD can audit and
// consume after validation succeeds.
type PromotionValidation struct {
	APIVersion      string                      `json:"apiVersion"`
	Kind            string                      `json:"kind"`
	Verdict         EvaluationVerdict           `json:"verdict"`
	EvidenceDigest  string                      `json:"evidenceDigest"`
	PolicyDigest    string                      `json:"policyDigest"`
	Candidate       TournamentCandidateIdentity `json:"candidate"`
	CandidateDigest string                      `json:"candidateDigest"`
	ConfigDigest    string                      `json:"configDigest"`
	LastKnownGood   TournamentCandidateIdentity `json:"lastKnownGood"`
	DatasetVersion  string                      `json:"datasetVersion"`
	Digests         EvaluationDigests           `json:"digests"`
	AcceptedResults []string                    `json:"acceptedResults"`
}

// CandidateIdentityDigest returns the canonical identity used to bind result
// artifacts to a promotion candidate.
func CandidateIdentityDigest(identity TournamentCandidateIdentity) string {
	return digestJSON(identity)
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

// ValidatePromotionEvidence validates a handoff without mutating Workflow CD
// or any production state.
func ValidatePromotionEvidence(evidence PromotionEvidence, policy PromotionPolicy) (PromotionValidation, error) {
	if err := validatePromotionPolicy(policy); err != nil {
		return PromotionValidation{}, fmt.Errorf("workflow gym promotion: invalid policy: %w", err)
	}
	if err := validatePromotionEvidenceHeader(evidence); err != nil {
		return PromotionValidation{}, fmt.Errorf("workflow gym promotion: invalid evidence: %w", err)
	}
	if evidence.Candidate != policy.Candidate || evidence.CandidateDigest != CandidateIdentityDigest(policy.Candidate) {
		return PromotionValidation{}, errors.New("workflow gym promotion: evidence candidate does not match policy candidate")
	}
	if evidence.ConfigDigest != policy.ConfigDigest {
		return PromotionValidation{}, errors.New("workflow gym promotion: evidence config digest does not match policy candidate")
	}
	if evidence.LastKnownGood != policy.LastKnownGood {
		return PromotionValidation{}, errors.New("workflow gym promotion: evidence last-known-good does not match policy")
	}
	if evidence.DatasetVersion != policy.DatasetVersion || evidence.Digests.Dataset != policy.Digests.Dataset {
		return PromotionValidation{}, errors.New("workflow gym promotion: evidence dataset does not match policy dataset")
	}
	if evidence.Digests.Evaluator != policy.Digests.Evaluator {
		return PromotionValidation{}, errors.New("workflow gym promotion: stale evaluator manifest")
	}
	if evidence.Digests.Environment != policy.Digests.Environment {
		return PromotionValidation{}, errors.New("workflow gym promotion: stale environment manifest")
	}

	results := []struct {
		name     string
		result   *PromotionResult
		required bool
	}{
		{name: "benchmark", result: &evidence.Benchmark, required: true},
		{name: "shadow", result: evidence.Shadow, required: policy.RequireShadow},
		{name: "holdout", result: evidence.Holdout, required: policy.RequireHoldout},
	}
	accepted := make([]string, 0, len(results))
	for _, item := range results {
		if item.result == nil {
			if item.required {
				return PromotionValidation{}, fmt.Errorf("workflow gym promotion: required %s evidence is missing", item.name)
			}
			continue
		}
		if err := validatePromotionResult(item.name, *item.result, evidence); err != nil {
			return PromotionValidation{}, err
		}
		accepted = append(accepted, item.name)
	}

	return PromotionValidation{
		APIVersion:      apiv1.WorkflowGymTaskAPIVersion,
		Kind:            PromotionValidationKind,
		Verdict:         EvaluationPass,
		EvidenceDigest:  digestJSON(evidence),
		PolicyDigest:    digestJSON(policy),
		Candidate:       evidence.Candidate,
		CandidateDigest: evidence.CandidateDigest,
		ConfigDigest:    evidence.ConfigDigest,
		LastKnownGood:   evidence.LastKnownGood,
		DatasetVersion:  evidence.DatasetVersion,
		Digests:         evidence.Digests,
		AcceptedResults: accepted,
	}, nil
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

func validatePromotionResult(name string, result PromotionResult, evidence PromotionEvidence) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"artifactDigest", result.ArtifactDigest},
		{"candidateDigest", result.CandidateDigest},
		{"datasetVersion", result.DatasetVersion},
		{"datasetDigest", result.DatasetDigest},
		{"evaluatorDigest", result.EvaluatorDigest},
		{"environmentDigest", result.EnvironmentDigest},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("workflow gym promotion: %s evidence %s is required", name, field.name)
		}
	}
	if result.CandidateDigest != evidence.CandidateDigest {
		return fmt.Errorf("workflow gym promotion: %s evidence is for the wrong candidate", name)
	}
	if result.DatasetVersion != evidence.DatasetVersion || result.DatasetDigest != evidence.Digests.Dataset {
		return fmt.Errorf("workflow gym promotion: %s evidence is for the wrong dataset", name)
	}
	if result.EvaluatorDigest != evidence.Digests.Evaluator {
		return fmt.Errorf("workflow gym promotion: %s evidence uses a stale evaluator manifest", name)
	}
	if result.EnvironmentDigest != evidence.Digests.Environment {
		return fmt.Errorf("workflow gym promotion: %s evidence uses a stale environment manifest", name)
	}
	if result.Samples < 1 {
		return fmt.Errorf("workflow gym promotion: %s evidence has no measured samples", name)
	}
	if result.Verdict != EvaluationPass {
		return fmt.Errorf("workflow gym promotion: %s evidence verdict is %q, require pass", name, result.Verdict)
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
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
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
