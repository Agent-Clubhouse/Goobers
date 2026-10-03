package decisiongate

import (
	"context"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/decider"
)

type recordingIntakeDecider struct {
	state intakeState
}

func (d *recordingIntakeDecider) Decide(_ context.Context, request decider.Request) (decider.Response, error) {
	d.state = request.State.(intakeState)
	yes := 0.99
	return decider.Response{Answers: map[string]decider.Answer{
		IntakeRiskQuestion: {Type: decider.KindNoul, Yes: &yes},
	}}, nil
}

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

func TestEvaluateIntakeRiskIncludesBoundedPeerBodies(t *testing.T) {
	recorder := &recordingIntakeDecider{}
	g, err := New(recorder, Config{
		Thresholds:   map[string]Threshold{IntakeRiskQuestion: DefaultIntakeRiskThreshold},
		CacheEntries: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	open := make([]IntakeItem, 10)
	for i := range open {
		open[i] = IntakeItem{
			ID:   string(rune('a' + i)),
			Body: strings.Repeat(string(rune('A'+i)), intakePeerBodyLimit+100),
		}
	}

	if _, err := g.EvaluateIntakeRisk(context.Background(), IntakeItem{ID: "candidate"}, open); err != nil {
		t.Fatal(err)
	}
	if len(recorder.state.OpenIssues) != len(open) {
		t.Fatalf("peer count = %d, want %d", len(recorder.state.OpenIssues), len(open))
	}
	total := 0
	for i, peer := range recorder.state.OpenIssues {
		total += len(peer.Body)
		want := intakePeerBodyLimit
		if i >= intakePeerBodiesLimit/intakePeerBodyLimit {
			want = 0
		}
		if len(peer.Body) != want {
			t.Fatalf("peer %d body length = %d, want %d", i, len(peer.Body), want)
		}
	}
	if total != intakePeerBodiesLimit {
		t.Fatalf("total peer body bytes = %d, want %d", total, intakePeerBodiesLimit)
	}
}
