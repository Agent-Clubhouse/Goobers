package knowledgerank

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
)

type scripted struct {
	mu     sync.Mutex
	scores map[string]float64
	err    error
	sent   []string
}

func (s *scripted) Decide(ctx context.Context, r decider.Request) (decider.Response, error) {
	st := r.State.(State)
	s.mu.Lock()
	s.sent = append(s.sent, st.Candidate)
	s.mu.Unlock()
	if s.err != nil {
		return decider.Response{}, s.err
	}
	sc := s.scores[st.Candidate]
	conf := 0.9
	return decider.Response{Model: "m-1", Answers: map[string]decider.Answer{
		QuestionName: {Type: decider.KindScore, Score: &sc, Confidence: &conf},
	}}, nil
}

func newRanker(t *testing.T, d decider.Decider, s Settings) *Ranker {
	t.Helper()
	g, err := decisiongate.New(d, decisiongate.Config{CallTimeout: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled = true
	r, err := New(g, s)
	if err != nil || r == nil {
		t.Fatalf("New: %v %v", r, err)
	}
	return r
}

func cands() []Candidate {
	return []Candidate{
		{ID: "a", Source: "docs", Text: "alpha"},
		{ID: "b", Source: "docs", Text: "bravo"},
		{ID: "c", Source: "runbook", Text: "charlie"},
		{ID: "d", Source: "docs", Text: "delta"},
	}
}

func TestOffByDefaultScoresNothing(t *testing.T) {
	d := &scripted{}
	g, _ := decisiongate.New(d, decisiongate.Config{}, nil)
	r, err := New(g, Settings{})
	if err != nil || r != nil {
		t.Fatalf("disabled settings gave %v %v", r, err)
	}
	if _, ok := r.Shadow(context.Background(), "t", cands()); ok || len(d.sent) != 0 {
		t.Fatal("nil ranker must be inert")
	}
}

func TestShadowRanksDropsAndKeepsBorderline(t *testing.T) {
	d := &scripted{scores: map[string]float64{"alpha": 1.0, "bravo": 0.2, "charlie": 1.9, "delta": 1.6}}
	rec, _ := newRanker(t, d, Settings{}).Shadow(context.Background(), "task", cands())
	if want := []string{"c", "d", "a"}; !slices.Equal(rec.Hypothetical, want) {
		t.Fatalf("hypothetical = %v, want %v", rec.Hypothetical, want)
	}
	if !slices.Equal(rec.Dropped, []string{"b"}) || !slices.Equal(rec.Existing, []string{"a", "b", "c", "d"}) {
		t.Fatalf("dropped=%v existing=%v", rec.Dropped, rec.Existing)
	}
	if rec.Model != "m-1" || rec.Thresholds.DropAtOrBelow != 0.5 || rec.Thresholds.NeededAtOrAbove != 1.5 || rec.TopN != 10 {
		t.Fatalf("provenance = %+v", rec)
	}
	if rec.Scored[0].Class != ClassRelated || rec.Scored[2].Class != ClassNeeded {
		t.Fatalf("classes = %+v", rec.Scored)
	}
}

func TestInternalKnowledgeNeedsExplicitOptIn(t *testing.T) {
	in := []Candidate{{ID: "pub", Text: "public note"}, {ID: "int", Internal: true, Text: "INTERNAL-SECRET"}}
	d := &scripted{scores: map[string]float64{"public note": 2, "INTERNAL-SECRET": 0}}
	rec, _ := newRanker(t, d, Settings{}).Shadow(context.Background(), "t", in)
	if slices.Contains(d.sent, "INTERNAL-SECRET") || rec.Scored[1].Skipped != SkipInternal {
		t.Fatalf("internal text reached the model without opt-in: sent=%v scored=%+v", d.sent, rec.Scored)
	}
	if !slices.Equal(rec.Hypothetical, []string{"pub", "int"}) || len(rec.Dropped) != 0 {
		t.Fatalf("unscored internal item must stay selected: %+v", rec)
	}
	d2 := &scripted{scores: d.scores}
	rec, _ = newRanker(t, d2, Settings{AllowInternalKnowledge: true}).Shadow(context.Background(), "t", in)
	if !slices.Contains(d2.sent, "INTERNAL-SECRET") || !slices.Equal(rec.Dropped, []string{"int"}) {
		t.Fatalf("opt-in should score internal: sent=%v rec=%+v", d2.sent, rec)
	}
}

func TestSourceAllowlistAndTopN(t *testing.T) {
	d := &scripted{scores: map[string]float64{"alpha": 2, "bravo": 2, "delta": 2}}
	rec, _ := newRanker(t, d, Settings{Sources: []string{"docs"}, TopN: 2}).Shadow(context.Background(), "t", cands())
	got := []string{rec.Scored[0].Skipped, rec.Scored[1].Skipped, rec.Scored[2].Skipped, rec.Scored[3].Skipped}
	if want := []string{"", "", SkipSource, SkipBeyondN}; !slices.Equal(got, want) {
		t.Fatalf("skips = %v, want %v", got, want)
	}
	if len(d.sent) != 2 || !slices.Equal(rec.Hypothetical, []string{"a", "b", "c", "d"}) {
		t.Fatalf("sent=%v hypothetical=%v", d.sent, rec.Hypothetical)
	}
}

func TestModelErrorKeepsEverything(t *testing.T) {
	rec, ok := newRanker(t, &scripted{err: errors.New("down")}, Settings{}).Shadow(context.Background(), "t", cands())
	if !ok || len(rec.Dropped) != 0 || !slices.Equal(rec.Hypothetical, rec.Existing) || rec.Scored[0].Error == "" {
		t.Fatalf("error must fall back to the existing selection: %+v", rec)
	}
}

func TestSettingsValidate(t *testing.T) {
	for name, s := range map[string]Settings{
		"topN too big":  {TopN: 99},
		"inverted":      {DropAtOrBelow: 1.8, NeededAtOrAbove: 1.2},
		"needed over 2": {NeededAtOrAbove: 2.5},
		"negative drop": {DropAtOrBelow: -1},
	} {
		if s.Validate() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if err := (Settings{}).Validate(); err != nil {
		t.Fatal(err)
	}
}
