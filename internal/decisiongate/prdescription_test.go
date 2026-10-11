package decisiongate

import (
	"context"
	"errors"
	"testing"
)

func TestEvaluatePRDescription(t *testing.T) {
	state := PRDescriptionState{
		Title: "Add retries", Description: "Retries transient requests.",
		ChangedFiles: []string{"client.go"}, AddedLines: 12, DeletedLines: 2,
		SizeBucket: "1-50", Patch: "+ retry()",
	}
	for _, tc := range []struct {
		name string
		yes  float64
		want Decision
	}{
		{name: "agrees", yes: 0.95, want: Yes},
		{name: "disagrees", yes: 0.05, want: No},
		{name: "unsure", yes: 0.5, want: Uncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cfg()
			cfg.Thresholds[PRDescriptionAgreementQuestion] = DefaultPRDescriptionAgreementThreshold
			g, err := New(&fake{yes: tc.yes}, cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := g.EvaluatePRDescription(context.Background(), state)
			if err != nil || got.Decision != tc.want || got.Probability != tc.yes {
				t.Fatalf("outcome = %+v, err = %v, want %s", got, err, tc.want)
			}
		})
	}
}

func TestEvaluatePRDescriptionErrorIsUncertain(t *testing.T) {
	cfg := cfg()
	cfg.Thresholds[PRDescriptionAgreementQuestion] = DefaultPRDescriptionAgreementThreshold
	g, err := New(&fake{err: errors.New("unavailable")}, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.EvaluatePRDescription(context.Background(), PRDescriptionState{})
	if err == nil || got.Decision != Uncertain {
		t.Fatalf("outcome = %+v, err = %v", got, err)
	}
}
