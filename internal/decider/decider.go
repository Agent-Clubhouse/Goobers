// Package decider is a provider-neutral client for decision ("System One")
// models: typed questions about a state that return calibrated probabilities
// instead of prose. It speaks the public Jev System One wire format
// (POST /v1/systemone), which any compatible deployment can serve.
//
// The package holds no endpoint, key, or model defaults. Callers supply them
// from their own configuration so that deployment-specific values stay out of
// this repository.
package decider

import (
	"context"
	"errors"
	"fmt"
)

// Decider answers typed questions about a state.
type Decider interface {
	Decide(ctx context.Context, req Request) (Response, error)
}

// Request is one evaluation. State is a string or any JSON-encodable value.
type Request struct {
	State     any
	Questions map[string]Question
}

// Kind is a question type.
type Kind string

// Question kinds on the wire.
const (
	KindNoul   Kind = "noul"
	KindChoice Kind = "choice"
	KindScore  Kind = "score"
)

// Question is one typed question. Construct with Noul, Choice, or Score.
type Question struct {
	Type         Kind
	Instructions any
	criteria     any
}

// NoulCriteria describes what a yes and a no mean.
type NoulCriteria struct {
	True  any `json:"true,omitempty"`
	False any `json:"false,omitempty"`
}

// Noul asks a yes/no question. Criteria may be nil.
func Noul(instructions any, criteria *NoulCriteria) Question {
	q := Question{Type: KindNoul, Instructions: instructions}
	if criteria != nil {
		q.criteria = criteria
	}
	return q
}

// Choice picks one option. Options map an option name to its description
// (nil when the name is self-explanatory).
func Choice(instructions any, options map[string]any) Question {
	return Question{Type: KindChoice, Instructions: instructions, criteria: options}
}

// Score rates along ordered levels, lowest first.
func Score(instructions any, levels []any) Question {
	return Question{Type: KindScore, Instructions: instructions, criteria: levels}
}

const (
	maxChoiceOptions = 255
	maxScoreLevels   = 10
)

func (q Question) validate() error {
	if q.Instructions == nil || q.Instructions == "" {
		return errors.New("instructions are required")
	}
	switch q.Type {
	case KindNoul:
		return nil
	case KindChoice:
		opts, _ := q.criteria.(map[string]any)
		if len(opts) < 2 || len(opts) > maxChoiceOptions {
			return fmt.Errorf("choice needs 2 to %d options, got %d", maxChoiceOptions, len(opts))
		}
	case KindScore:
		levels, _ := q.criteria.([]any)
		if len(levels) < 2 || len(levels) > maxScoreLevels {
			return fmt.Errorf("score needs 2 to %d levels, got %d", maxScoreLevels, len(levels))
		}
	default:
		return fmt.Errorf("unknown question type %q", q.Type)
	}
	return nil
}

// Answer is the decoded answer to one question. Only the fields for its Type
// are set.
type Answer struct {
	Type Kind `json:"type"`
	// Yes is the probability of "yes" for a noul answer, 0 to 1.
	Yes *float64 `json:"noul,omitempty"`
	// Choice is the highest-probability option for a choice answer.
	Choice string `json:"choice,omitempty"`
	// Score is the probability-weighted level for a score answer.
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Confidence is reported for choice and score answers, 0 to 1.
	Confidence *float64 `json:"confidence,omitempty"`
}

// Usage is the token accounting for a request.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response holds one answer per requested question id.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}
