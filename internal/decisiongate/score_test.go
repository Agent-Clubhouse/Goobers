package decisiongate

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/decider"
)

type scoreFake struct {
	score float64
	calls atomic.Int32
}

func (s *scoreFake) Decide(_ context.Context, r decider.Request) (decider.Response, error) {
	s.calls.Add(1)
	out := decider.Response{Model: "m-1", Answers: map[string]decider.Answer{}}
	for id := range r.Questions {
		v := s.score
		out.Answers[id] = decider.Answer{Type: decider.KindScore, Score: &v}
	}
	return out, nil
}

func TestJudgeScoreCachesAndReportsModel(t *testing.T) {
	f := &scoreFake{score: 1.4}
	g, err := New(f, Config{CacheEntries: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := decider.Score("how useful?", []any{"no", "some", "yes"})
	a, err := g.JudgeScore(context.Background(), "rel", "state", q)
	if err != nil || a.Score != 1.4 || a.Model != "m-1" || a.Cached {
		t.Fatalf("first = %+v %v", a, err)
	}
	b, _ := g.JudgeScore(context.Background(), "rel", "state", q)
	if !b.Cached || b.Score != 1.4 || b.Model != "m-1" || f.calls.Load() != 1 {
		t.Fatalf("second = %+v calls=%d", b, f.calls.Load())
	}
	if _, err := g.JudgeScore(context.Background(), "rel", "state", decider.Noul("x", nil)); err == nil {
		t.Fatal("a non-score question must be refused")
	}
}
