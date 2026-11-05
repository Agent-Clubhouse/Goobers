package decisiongate

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/decider"
)

// JudgeChoice asks a pick-one question and thresholds the answer. The top
// option is accepted (Decision Yes, Outcome.Choice set) only when its
// probability reaches the question's Accept threshold and, when configured,
// the answer confidence reaches MinConfidence; otherwise the outcome is
// Uncertain with Choice empty. Any error is Uncertain and returned, so a
// caller that ignores the error still gets the safe value.
func (g *Gate) JudgeChoice(ctx context.Context, name string, state any, q decider.Question) (Outcome, error) {
	start := g.now()
	uncertain := Outcome{Name: name, Decision: Uncertain}
	th, ok := g.cfg.Thresholds[name]
	if !ok {
		return g.finish(name, "", uncertain, fmt.Errorf("decisiongate: no threshold configured for %q", name), start)
	}
	if q.Type != decider.KindChoice {
		return g.finish(name, "", uncertain, fmt.Errorf("decisiongate: %q is not a choice question", name), start)
	}
	digest, err := digestOf(name, state, q)
	if err != nil {
		return g.finish(name, "", uncertain, err, start)
	}
	if o, hit := g.lookup(digest); hit {
		o.Cached = true
		return g.finish(name, digest, o, nil, start)
	}

	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-ctx.Done():
		return g.finish(name, digest, uncertain, ctx.Err(), start)
	}
	cctx, cancel := context.WithTimeout(ctx, g.cfg.CallTimeout)
	defer cancel()
	resp, err := g.d.Decide(cctx, decider.Request{State: state, Questions: map[string]decider.Question{name: q}})
	if err != nil {
		return g.finish(name, digest, uncertain, err, start)
	}
	ans, ok := resp.Answers[name]
	if !ok || ans.Choice == "" {
		return g.finish(name, digest, uncertain, fmt.Errorf("decisiongate: no choice answer for %q", name), start)
	}
	o := Outcome{Name: name, Decision: Uncertain, Probability: ans.Probabilities[ans.Choice]}
	if ans.Confidence != nil {
		o.Confidence = *ans.Confidence
	}
	if o.Probability >= th.Accept && (g.cfg.MinConfidence == 0 || o.Confidence >= g.cfg.MinConfidence) {
		o.Decision, o.Choice = Yes, ans.Choice
	}
	g.store(digest, o)
	return g.finish(name, digest, o, nil, start)
}
