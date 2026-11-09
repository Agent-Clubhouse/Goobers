package decisiongate

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/decider"
)

// ScoreOutcome is one judged score question. Score is the probability-weighted
// level, 0 (lowest) through levels-1.
type ScoreOutcome struct {
	Name       string
	Score      float64
	Confidence float64
	Cached     bool
	// Model is the model version the decider reported, for reproducibility.
	Model string
}

// JudgeScore asks a graded question. Unlike the noul and choice paths it
// applies no threshold: the caller owns the policy for each level. The call
// shares the gate's cache (which stores the score in Outcome.Probability and
// the model version in Outcome.Choice), concurrency limit and timeout. On any
// error the zero outcome is returned with the error, so callers must keep
// their existing behavior.
func (g *Gate) JudgeScore(ctx context.Context, name string, state any, q decider.Question) (ScoreOutcome, error) {
	start := g.now()
	fail := func(digest string, err error) (ScoreOutcome, error) {
		g.finish(name, digest, Outcome{Name: name, Decision: Uncertain}, err, start)
		return ScoreOutcome{Name: name}, err
	}
	if q.Type != decider.KindScore {
		return fail("", fmt.Errorf("decisiongate: %q is not a score question", name))
	}
	digest, err := digestOf(name, state, q)
	if err != nil {
		return fail("", err)
	}
	if o, hit := g.lookup(digest); hit {
		g.finish(name, digest, o, nil, start)
		return ScoreOutcome{Name: name, Score: o.Probability, Confidence: o.Confidence, Cached: true, Model: o.Choice}, nil
	}
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-ctx.Done():
		return fail(digest, ctx.Err())
	}
	cctx, cancel := context.WithTimeout(ctx, g.cfg.CallTimeout)
	defer cancel()
	resp, err := g.d.Decide(cctx, decider.Request{State: state, Questions: map[string]decider.Question{name: q}})
	if err != nil {
		return fail(digest, err)
	}
	ans, ok := resp.Answers[name]
	if !ok || ans.Score == nil {
		return fail(digest, fmt.Errorf("decisiongate: no score answer for %q", name))
	}
	out := ScoreOutcome{Name: name, Score: *ans.Score, Model: resp.Model}
	if ans.Confidence != nil {
		out.Confidence = *ans.Confidence
	}
	g.store(digest, Outcome{Name: name, Decision: Uncertain, Probability: out.Score, Confidence: out.Confidence, Choice: out.Model})
	g.finish(name, digest, Outcome{Name: name, Decision: Uncertain, Probability: out.Score, Confidence: out.Confidence}, nil, start)
	return out, nil
}
