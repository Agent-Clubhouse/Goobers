package decisioneval

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

// Judge is the baseline: a free-form LLM asked the same yes/no question.
type Judge interface {
	Judge(ctx context.Context, instructions, state string) (decisiongate.Decision, error)
}

// Verdict is one arm's answer for one item. Prob is nil when the arm gives no
// probability (the baseline judge).
type Verdict struct {
	Decision decisiongate.Decision
	Prob     *float64
	Err      error
}

// Arm answers an item.
type Arm interface {
	Name() string
	Answer(ctx context.Context, it Item) Verdict
}

// ReplayDecider is a decider.Decider that returns the recorded systemOne
// probability for each state text. Unknown states error.
type ReplayDecider struct{ byText map[string]float64 }

// NewReplayDecider indexes the recorded probabilities of items.
func NewReplayDecider(items []Item) *ReplayDecider {
	m := map[string]float64{}
	for _, it := range items {
		if it.SystemOne != nil {
			m[it.Text] = *it.SystemOne
		}
	}
	return &ReplayDecider{byText: m}
}

// Decide implements decider.Decider.
func (r *ReplayDecider) Decide(_ context.Context, req decider.Request) (decider.Response, error) {
	text, _ := req.State.(string)
	p, ok := r.byText[text]
	if !ok {
		return decider.Response{}, errors.New("no recorded answer for state")
	}
	out := decider.Response{Model: "replay", Answers: map[string]decider.Answer{}}
	for id := range req.Questions {
		y := p
		out.Answers[id] = decider.Answer{Type: decider.KindNoul, Yes: &y}
	}
	return out, nil
}

// SystemOneArm runs the real decisiongate.Gate over any decider.Decider.
type SystemOneArm struct {
	gate     *decisiongate.Gate
	question string
	q        decider.Question
}

// NewSystemOneArm builds the System One arm with the given thresholds. Caching
// is disabled so repeated runs exercise the decider each time.
func NewSystemOneArm(d decider.Decider, f Fixture, th decisiongate.Threshold) (*SystemOneArm, error) {
	cfg := decisiongate.Config{Thresholds: map[string]decisiongate.Threshold{f.Question: th}}
	g, err := decisiongate.New(d, cfg, nil)
	if err != nil {
		return nil, fmt.Errorf("system one arm: %w", err)
	}
	return &SystemOneArm{gate: g, question: f.Question, q: decider.Noul(f.Instructions, nil)}, nil
}

// Name implements Arm.
func (a *SystemOneArm) Name() string { return "system-one" }

// Answer implements Arm.
func (a *SystemOneArm) Answer(ctx context.Context, it Item) Verdict {
	o, err := a.gate.JudgeNoul(ctx, a.question, it.Text, a.q)
	if err != nil {
		return Verdict{Decision: decisiongate.Uncertain, Err: err}
	}
	p := o.Probability
	return Verdict{Decision: o.Decision, Prob: &p}
}

// JudgeArm adapts a Judge to an Arm.
type JudgeArm struct {
	J            Judge
	Instructions string
}

// Name implements Arm.
func (a *JudgeArm) Name() string { return "llm-judge" }

// Answer implements Arm.
func (a *JudgeArm) Answer(ctx context.Context, it Item) Verdict {
	d, err := a.J.Judge(ctx, a.Instructions, it.Text)
	if err != nil {
		return Verdict{Decision: decisiongate.Uncertain, Err: err}
	}
	return Verdict{Decision: d}
}

// ReplayJudge returns the recorded judge verdict (yes, no or uncertain) for
// each state text.
type ReplayJudge struct{ byText map[string]string }

// NewReplayJudge indexes the recorded judge verdicts of items.
func NewReplayJudge(items []Item) *ReplayJudge {
	m := map[string]string{}
	for _, it := range items {
		if it.Judge != "" {
			m[it.Text] = it.Judge
		}
	}
	return &ReplayJudge{byText: m}
}

// Judge implements Judge.
func (r *ReplayJudge) Judge(_ context.Context, _, state string) (decisiongate.Decision, error) {
	switch r.byText[state] {
	case "yes":
		return decisiongate.Yes, nil
	case "no":
		return decisiongate.No, nil
	case "uncertain":
		return decisiongate.Uncertain, nil
	}
	return decisiongate.Uncertain, errors.New("no recorded judge answer for state")
}
