package decisiongate

import (
	"context"
	"testing"
)

func TestEvaluatePublicationUsesThresholds(t *testing.T) {
	for name, tc := range map[string]struct {
		yes  float64
		want Decision
	}{
		"review":    {yes: 0.95, want: Yes},
		"clear":     {yes: 0.05, want: No},
		"ambiguous": {yes: 0.5, want: Uncertain},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := New(&fake{yes: tc.yes}, Config{Thresholds: map[string]Threshold{
				PublicationLeakQuestion: DefaultPublicationLeakThreshold,
			}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := g.EvaluatePublication(context.Background(), "pull-request", "Public title", "Public body")
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != tc.want {
				t.Fatalf("decision = %q, want %q", got.Decision, tc.want)
			}
		})
	}
}
