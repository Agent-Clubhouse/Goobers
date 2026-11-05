package creditgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

// ModelAssistThresholdKey is the decisiongate threshold key for the
// ClassUnknown suggestion question.
const ModelAssistThresholdKey = "creditgraph_unknown_class"

// DefaultModelAssistThreshold keeps a weak suggestion as unknown.
var DefaultModelAssistThreshold = decisiongate.Threshold{Accept: 0.8, Reject: 0.2}

// ChoiceJudge is the part of decisiongate.Gate the assist needs.
type ChoiceJudge interface {
	JudgeChoice(ctx context.Context, name string, state any, q decider.Question) (decisiongate.Outcome, error)
}

// ModelAssistedFinding is a labeled advisory suggestion for a ClassUnknown
// cause. It is a statistical estimate, never a rule-based finding: it lives
// beside the Attribution, never inside it, and never changes a Share, Score,
// Uncertainty, Confidence or Class. Suggested is ClassUnknown whenever the
// answer is below threshold or the model failed.
type ModelAssistedFinding struct {
	NodeID         string       `json:"nodeId"`
	Stage          string       `json:"stage,omitempty"`
	Suggested      FailureClass `json:"suggested"`
	Probability    float64      `json:"probability"`
	Confidence     float64      `json:"confidence"`
	EvidenceDigest string       `json:"evidenceDigest"`
	Cached         bool         `json:"cached,omitempty"`
	Error          string       `json:"error,omitempty"`
}

// suggestableClasses are the classes a model may suggest. Unknown is the
// refusal and is therefore not an option.
var suggestableClasses = map[FailureClass]string{
	ClassBadToolChoice:     "A tool call named the wrong tool for the work.",
	ClassBadToolResult:     "A tool ran and reported an unsuccessful result.",
	ClassBadInterpretation: "Tool results were good and what was made of them was not.",
	ClassWeakInstructions:  "The instructions produced no tool calls and no evidence.",
	ClassRouting:           "The model that ran is not the model the subagent requested.",
	ClassModel:             "The model produced no output.",
	ClassTopology:          "A declared dependency on a subagent never ran.",
	ClassEnvironment:       "The harness or environment raised the error, not an agent decision.",
}

func assistQuestion() decider.Question {
	options := make(map[string]any, len(suggestableClasses))
	for class, desc := range suggestableClasses {
		options[string(class)] = desc
	}
	return decider.Choice("Which failure class best explains this failed workflow stage, given only the recorded evidence?", options)
}

type assistState struct {
	Stage       string   `json:"stage,omitempty"`
	ErrorCode   string   `json:"errorCode,omitempty"`
	Summary     string   `json:"summary"`
	Assumptions []string `json:"assumptions,omitempty"`
	Evidence    []string `json:"evidence,omitempty"`
}

func assistStateOf(cause CauseFinding) (assistState, string) {
	st := assistState{Stage: cause.Stage, ErrorCode: cause.ErrorCode, Summary: cause.Summary, Assumptions: cause.Assumptions, Evidence: cause.Evidence}
	b, _ := json.Marshal(st)
	sum := sha256.Sum256(b)
	return st, "sha256:" + hex.EncodeToString(sum[:])
}

// SuggestUnknownCauses asks judge about every ClassUnknown cause in a and
// returns one advisory finding per cause, in cause order. Rule-based causes
// are never sent to the model and a is never modified. A nil judge returns
// nil, so the feature is off unless a caller supplies a gate.
func SuggestUnknownCauses(ctx context.Context, judge ChoiceJudge, a Attribution) []ModelAssistedFinding {
	if judge == nil {
		return nil
	}
	q := assistQuestion()
	var out []ModelAssistedFinding
	for _, cause := range a.CausesOfClass(ClassUnknown) {
		state, digest := assistStateOf(cause)
		f := ModelAssistedFinding{NodeID: cause.NodeID, Stage: cause.Stage, Suggested: ClassUnknown, EvidenceDigest: digest}
		o, err := judge.JudgeChoice(ctx, ModelAssistThresholdKey, state, q)
		f.Probability, f.Confidence, f.Cached = o.Probability, o.Confidence, o.Cached
		if err != nil {
			f.Error = err.Error()
		} else if class := FailureClass(o.Choice); o.Decision == decisiongate.Yes {
			if _, ok := suggestableClasses[class]; ok {
				f.Suggested = class
			}
		}
		out = append(out, f)
	}
	return out
}
