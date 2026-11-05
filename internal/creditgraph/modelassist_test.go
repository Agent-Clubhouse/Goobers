package creditgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

type choiceDecider struct {
	choice string
	prob   float64
	err    error
	calls  int
}

func (d *choiceDecider) Decide(_ context.Context, r decider.Request) (decider.Response, error) {
	d.calls++
	if d.err != nil {
		return decider.Response{}, d.err
	}
	conf := 0.9
	out := decider.Response{Answers: map[string]decider.Answer{}}
	for id := range r.Questions {
		out.Answers[id] = decider.Answer{Type: decider.KindChoice, Choice: d.choice, Confidence: &conf,
			Probabilities: map[string]float64{d.choice: d.prob}}
	}
	return out, nil
}

func assistGate(t *testing.T, d decider.Decider) *decisiongate.Gate {
	t.Helper()
	g, err := decisiongate.New(d, decisiongate.Config{Thresholds: map[string]decisiongate.Threshold{
		ModelAssistThresholdKey: DefaultModelAssistThreshold,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func assistAttribution() Attribution {
	return Attribution{Causes: []CauseFinding{
		{Class: ClassEnvironment, NodeID: "n1", Summary: "rule based"},
		{Class: ClassUnknown, NodeID: "n2", Stage: "implement", Summary: "no rule fired", Evidence: []string{"e1"}},
	}}
}

func TestSuggestUnknownCausesOnlyAsksAboutUnknownAndKeepsAttribution(t *testing.T) {
	a := assistAttribution()
	before, _ := json.Marshal(a)
	d := &choiceDecider{choice: string(ClassBadInterpretation), prob: 0.95}
	got := SuggestUnknownCauses(context.Background(), assistGate(t, d), a)
	if d.calls != 1 || len(got) != 1 {
		t.Fatalf("calls=%d findings=%d, want one of each", d.calls, len(got))
	}
	if got[0].NodeID != "n2" || got[0].Suggested != ClassBadInterpretation || got[0].EvidenceDigest == "" || got[0].Probability != 0.95 {
		t.Fatalf("finding = %+v", got[0])
	}
	if after, _ := json.Marshal(a); !bytes.Equal(before, after) {
		t.Fatal("attribution was modified")
	}
}

func TestSuggestUnknownCausesStaysUnknownBelowThresholdOrOnError(t *testing.T) {
	for name, d := range map[string]*choiceDecider{
		"low":   {choice: string(ClassModel), prob: 0.5},
		"error": {err: errors.New("down")},
		"bogus": {choice: "unknown", prob: 0.99},
	} {
		t.Run(name, func(t *testing.T) {
			got := SuggestUnknownCauses(context.Background(), assistGate(t, d), assistAttribution())
			if len(got) != 1 || got[0].Suggested != ClassUnknown {
				t.Fatalf("got %+v, want unknown", got)
			}
			if name == "error" && got[0].Error == "" {
				t.Fatal("error not recorded")
			}
		})
	}
	if SuggestUnknownCauses(context.Background(), nil, assistAttribution()) != nil {
		t.Fatal("nil judge must be a no-op")
	}
}
