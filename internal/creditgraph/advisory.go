package creditgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

const (
	modelFailureClassQuestion = "creditgraph.failure-class"
	modelAssistedLabel        = "model-assisted"
	modelAssistedPolicy       = "creditgraph.failure-class/v1"
	modelAssistedProbability  = 0.75
	modelAssistedConfidence   = 0.75
)

// AdvisoryClassifier pins the model used by the shadow classifier. Thresholds
// are fixed by the versioned policy above rather than instance configuration.
type AdvisoryClassifier struct {
	Gate  *decisiongate.Gate
	Model string
}

// ModelAssistedFinding is a shadow-only suggestion for a rule-based unknown
// cause. It is separate from Attribution so it cannot affect credit assignment.
type ModelAssistedFinding struct {
	Label          string       `json:"label"`
	Class          FailureClass `json:"class"`
	SuggestedClass FailureClass `json:"suggestedClass,omitempty"`
	NodeID         string       `json:"nodeId"`
	Stage          string       `json:"stage,omitempty"`
	RunID          string       `json:"runId,omitempty"`
	Probability    float64      `json:"probability"`
	Confidence     float64      `json:"confidence"`
	Model          string       `json:"model"`
	PinnedModel    string       `json:"pinnedModel"`
	EvidenceDigest string       `json:"evidenceDigest"`
	Cached         bool         `json:"cached,omitempty"`
}

type modelFailureState struct {
	Policy        string         `json:"policy"`
	PinnedModel   string         `json:"pinnedModel"`
	RunID         string         `json:"runId,omitempty"`
	RootID        string         `json:"rootId"`
	Outcome       string         `json:"outcome,omitempty"`
	Finding       CauseFinding   `json:"finding"`
	Contributions []Contribution `json:"contributions"`
}

var failureClassChoice = decider.Choice(
	"Choose the most likely failure class supported by the recorded evidence. Choose unknown when the evidence does not support attribution.",
	map[string]any{
		string(ClassBadToolChoice):     "the wrong tool was selected",
		string(ClassBadToolResult):     "a tool reported an unsuccessful result",
		string(ClassBadInterpretation): "successful tool results were interpreted incorrectly",
		string(ClassWeakInstructions):  "instructions produced no useful work or evidence",
		string(ClassRouting):           "the executed model differed from the requested model",
		string(ClassModel):             "the model invocation produced no output",
		string(ClassTopology):          "agent dependency wiring prevented required work",
		string(ClassEnvironment):       "the harness or execution environment failed",
		string(ClassUnknown):           "the evidence does not support any other class",
	},
)

// ModelAssistedEvidenceDigest identifies the evidence and pinned policy used
// for one suggestion, allowing durable replay without another model call.
func ModelAssistedEvidenceDigest(classifier AdvisoryClassifier, attribution Attribution, cause CauseFinding) (string, error) {
	state, err := modelFailureEvidence(classifier, attribution, cause)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("creditgraph: encode model-assisted evidence: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func modelFailureEvidence(classifier AdvisoryClassifier, attribution Attribution, cause CauseFinding) (modelFailureState, error) {
	if classifier.Gate == nil {
		return modelFailureState{}, errors.New("creditgraph: decision gate is required")
	}
	if classifier.Model == "" {
		return modelFailureState{}, errors.New("creditgraph: pinned advisory model is required")
	}
	return modelFailureState{
		Policy: modelAssistedPolicy, PinnedModel: classifier.Model,
		RunID: attribution.RunID, RootID: attribution.RootID, Outcome: attribution.Outcome,
		Finding: cause, Contributions: attribution.Contributions,
	}, nil
}

// ClassifyUnknownShadow asks for advisory classifications only where the
// deterministic rules refused to attribute a cause. It never mutates input.
func ClassifyUnknownShadow(ctx context.Context, classifier AdvisoryClassifier, attribution Attribution) ([]ModelAssistedFinding, error) {
	if classifier.Gate == nil {
		return nil, errors.New("creditgraph: decision gate is required")
	}
	if classifier.Model == "" {
		return nil, errors.New("creditgraph: pinned advisory model is required")
	}
	var findings []ModelAssistedFinding
	for _, cause := range attribution.Causes {
		if cause.Class != ClassUnknown {
			continue
		}
		state, err := modelFailureEvidence(classifier, attribution, cause)
		if err != nil {
			return nil, err
		}
		digest, err := ModelAssistedEvidenceDigest(classifier, attribution, cause)
		if err != nil {
			return nil, err
		}
		outcome, err := classifier.Gate.JudgeChoice(ctx, modelFailureClassQuestion, state, failureClassChoice)
		if err != nil {
			return nil, fmt.Errorf("creditgraph: classify unknown cause for %s: %w", cause.NodeID, err)
		}
		suggested := FailureClass(outcome.Choice)
		class := suggested
		if outcome.Model != classifier.Model ||
			outcome.Probability < modelAssistedProbability ||
			outcome.Confidence < modelAssistedConfidence {
			class = ClassUnknown
		}
		findings = append(findings, ModelAssistedFinding{
			Label: modelAssistedLabel, Class: class, SuggestedClass: suggested,
			NodeID: cause.NodeID, Stage: cause.Stage, RunID: attribution.RunID,
			Probability: outcome.Probability, Confidence: outcome.Confidence,
			Model: outcome.Model, PinnedModel: classifier.Model,
			EvidenceDigest: digest, Cached: outcome.Cached,
		})
	}
	return findings, nil
}
