package readservice

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

type faultDomainDecider struct {
	requests []decider.Request
}

func (d *faultDomainDecider) Decide(_ context.Context, request decider.Request) (decider.Response, error) {
	d.requests = append(d.requests, request)
	answers := map[string]decider.Answer{}
	for name, question := range request.Questions {
		confidence := 0.92
		switch question.Type {
		case decider.KindChoice:
			answers[name] = decider.Answer{
				Type: decider.KindChoice, Choice: string(creditgraph.FaultDomainProductRuntime),
				Confidence: &confidence,
			}
		case decider.KindScore:
			score := 2.4
			answers[name] = decider.Answer{Type: decider.KindScore, Score: &score, Confidence: &confidence}
		}
	}
	return decider.Response{Answers: answers}, nil
}

func TestFaultDomainAdvisorJudgesOnlyAttributorText(t *testing.T) {
	decisions := &faultDomainDecider{}
	gate, err := decisiongate.New(decisions, decisiongate.Config{MinConfidence: 0.8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	advice, err := (faultDomainAdvisor{gate: gate}).ClassifyFaultDomain(
		context.Background(),
		"The harness or environment reported a shared runtime failure.",
	)
	if err != nil {
		t.Fatal(err)
	}
	if advice.Domain != creditgraph.FaultDomainProductRuntime || advice.Confidence != 0.92 ||
		advice.Quality == nil || *advice.Quality < 0.799 || *advice.Quality > 0.801 {
		t.Fatalf("advice = %+v", advice)
	}
	if len(decisions.requests) != 2 {
		t.Fatalf("requests = %d, want domain and quality", len(decisions.requests))
	}
	for _, request := range decisions.requests {
		data, err := json.Marshal(request.State)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != `{"attributorText":"The harness or environment reported a shared runtime failure."}` {
			t.Fatalf("model state leaked deterministic facts: %s", data)
		}
	}
}
