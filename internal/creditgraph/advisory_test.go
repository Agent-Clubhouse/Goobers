package creditgraph

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

type advisoryDecider struct {
	choice      FailureClass
	probability float64
	confidence  float64
	model       string
	calls       atomic.Int32
}

func (d *advisoryDecider) Decide(_ context.Context, request decider.Request) (decider.Response, error) {
	d.calls.Add(1)
	answers := make(map[string]decider.Answer, len(request.Questions))
	for name := range request.Questions {
		confidence := d.confidence
		probabilities := map[string]float64{
			string(d.choice):     d.probability,
			string(ClassUnknown): 1 - d.probability,
		}
		answers[name] = decider.Answer{
			Type: decider.KindChoice, Choice: string(d.choice),
			Confidence: &confidence, Probabilities: probabilities,
		}
	}
	return decider.Response{Model: d.model, Answers: answers}, nil
}

func TestClassifyUnknownShadowRecordsAdvisoryWithoutChangingAttribution(t *testing.T) {
	attribution := Attribution{
		RunID: "run-1", RootID: "outcome", Outcome: "failed",
		Contributions: []Contribution{{NodeID: "stage:act#1", Share: 1, Score: -1, Uncertainty: 0.4}},
		Causes: []CauseFinding{{
			Class: ClassUnknown, NodeID: "stage:act#1", Stage: "act",
			Confidence: 0.4, Summary: "no recorded signal distinguishes a cause",
		}},
	}
	before := attribution
	d := &advisoryDecider{
		choice: ClassEnvironment, probability: 0.82, confidence: 0.76,
		model: "failure-classifier-v1",
	}
	gate, err := decisiongate.New(d, decisiongate.Config{CacheEntries: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}

	findings, err := ClassifyUnknownShadow(context.Background(), AdvisoryClassifier{
		Gate: gate, Model: "failure-classifier-v1",
	}, attribution)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(attribution, before) {
		t.Fatalf("deterministic attribution changed: before=%+v after=%+v", before, attribution)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one", findings)
	}
	finding := findings[0]
	if finding.Label != modelAssistedLabel || finding.Class != ClassEnvironment ||
		finding.SuggestedClass != ClassEnvironment || finding.Probability != 0.82 ||
		finding.Confidence != 0.76 || finding.Model != "failure-classifier-v1" ||
		finding.PinnedModel != "failure-classifier-v1" ||
		finding.EvidenceDigest == "" {
		t.Fatalf("finding = %+v", finding)
	}
}

func TestClassifyUnknownShadowEnforcesFixedPolicyBoundaries(t *testing.T) {
	attribution := Attribution{RootID: "outcome", Causes: []CauseFinding{
		{Class: ClassBadToolResult, NodeID: "tool-result"},
		{Class: ClassUnknown, NodeID: "stage:act#1"},
	}}
	for _, test := range []struct {
		name        string
		probability float64
		confidence  float64
		model       string
		want        FailureClass
	}{
		{name: "at thresholds", probability: modelAssistedProbability, confidence: modelAssistedConfidence, model: "pinned", want: ClassModel},
		{name: "probability below", probability: modelAssistedProbability - 0.01, confidence: 1, model: "pinned", want: ClassUnknown},
		{name: "confidence below", probability: 1, confidence: modelAssistedConfidence - 0.01, model: "pinned", want: ClassUnknown},
		{name: "model mismatch", probability: 1, confidence: 1, model: "other", want: ClassUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := &advisoryDecider{
				choice: ClassModel, probability: test.probability,
				confidence: test.confidence, model: test.model,
			}
			gate, err := decisiongate.New(d, decisiongate.Config{MinConfidence: 0, CacheEntries: 4}, nil)
			if err != nil {
				t.Fatal(err)
			}
			findings, err := ClassifyUnknownShadow(context.Background(), AdvisoryClassifier{
				Gate: gate, Model: "pinned",
			}, attribution)
			if err != nil {
				t.Fatal(err)
			}
			if d.calls.Load() != 1 || len(findings) != 1 {
				t.Fatalf("calls = %d findings = %+v, want only the unknown cause classified", d.calls.Load(), findings)
			}
			if findings[0].Class != test.want || findings[0].SuggestedClass != ClassModel {
				t.Fatalf("finding = %+v, want class %q with recorded suggestion", findings[0], test.want)
			}
		})
	}
}

func TestModelAssistedEvidenceDigestPinsModel(t *testing.T) {
	attribution := Attribution{RootID: "outcome", Causes: []CauseFinding{{
		Class: ClassUnknown, NodeID: "stage:act#1",
	}}}
	gate, err := decisiongate.New(&advisoryDecider{}, decisiongate.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ModelAssistedEvidenceDigest(
		AdvisoryClassifier{Gate: gate, Model: "v1"}, attribution, attribution.Causes[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ModelAssistedEvidenceDigest(
		AdvisoryClassifier{Gate: gate, Model: "v2"}, attribution, attribution.Causes[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("digest = %q for both pinned models", first)
	}
}
