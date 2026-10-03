package decisiongate

import (
	"context"
	"testing"
)

func TestEvaluateIntakeRiskExcludesCandidateFromPeers(t *testing.T) {
	f := &fake{yes: 0.99}
	g, err := New(f, Config{
		Thresholds:   map[string]Threshold{IntakeRiskQuestion: DefaultIntakeRiskThreshold},
		CacheEntries: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := IntakeItem{ID: "42", Title: "candidate"}
	open := []IntakeItem{candidate, {ID: "41", Title: "prerequisite"}}

	outcome, err := g.EvaluateIntakeRisk(context.Background(), candidate, open)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision != Yes || outcome.Probability != 0.99 {
		t.Fatalf("outcome = %+v, want a flagged intake risk", outcome)
	}

	withoutSelf, err := g.EvaluateIntakeRisk(context.Background(), candidate, open[1:])
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Name != withoutSelf.Name || !withoutSelf.Cached || f.calls.Load() != 1 {
		t.Fatalf("candidate exclusion changed evaluation: first=%+v second=%+v calls=%d", outcome, withoutSelf, f.calls.Load())
	}
}
