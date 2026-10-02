package decisiongate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/decider"
)

type fake struct {
	yes   float64
	err   error
	calls atomic.Int32
	delay time.Duration
	live  atomic.Int32
	peak  atomic.Int32
}

func (f *fake) Decide(ctx context.Context, r decider.Request) (decider.Response, error) {
	f.calls.Add(1)
	n := f.live.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer f.live.Add(-1)
	time.Sleep(f.delay)
	if f.err != nil {
		return decider.Response{}, f.err
	}
	out := decider.Response{Answers: map[string]decider.Answer{}}
	for id := range r.Questions {
		y := f.yes
		out.Answers[id] = decider.Answer{Type: decider.KindNoul, Yes: &y}
	}
	return out, nil
}

func cfg() Config {
	return Config{Thresholds: map[string]Threshold{ClaimQuestion: {Accept: 0.9, Reject: 0.1}}, CacheEntries: 8}
}

func TestThresholds(t *testing.T) {
	for _, tc := range []struct {
		yes  float64
		want Decision
	}{{0.95, Yes}, {0.9, Yes}, {0.5, Uncertain}, {0.1, No}, {0.01, No}} {
		g, _ := New(&fake{yes: tc.yes}, cfg(), nil)
		o, err := g.JudgeNoul(context.Background(), ClaimQuestion, "x", claimQuestion)
		if err != nil || o.Decision != tc.want {
			t.Fatalf("p=%v got %v err=%v want %v", tc.yes, o.Decision, err, tc.want)
		}
	}
}

func TestErrorIsUncertain(t *testing.T) {
	g, _ := New(&fake{err: errors.New("boom")}, cfg(), nil)
	o, err := g.JudgeNoul(context.Background(), ClaimQuestion, "x", claimQuestion)
	if err == nil || o.Decision != Uncertain {
		t.Fatalf("got %v %v", o.Decision, err)
	}
}

func TestMissingThresholdFailsClosed(t *testing.T) {
	g, _ := New(&fake{yes: 1}, Config{}, nil)
	o, err := g.JudgeNoul(context.Background(), "nope", "x", claimQuestion)
	if err == nil || o.Decision != Uncertain {
		t.Fatalf("got %v %v", o.Decision, err)
	}
}

func TestCacheAndObserver(t *testing.T) {
	f := &fake{yes: 0.99}
	var events []Event
	g, _ := New(f, cfg(), func(e Event) { events = append(events, e) })
	for i := 0; i < 3; i++ {
		if _, err := g.JudgeNoul(context.Background(), ClaimQuestion, "same", claimQuestion); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", f.calls.Load())
	}
	if len(events) != 3 || !events[1].Outcome.Cached || events[0].StateDigest == "" {
		t.Fatalf("events = %+v", events)
	}
}

func TestCacheEvictsAndExpires(t *testing.T) {
	f := &fake{yes: 0.99}
	c := cfg()
	c.CacheEntries = 1
	g, _ := New(f, c, nil)
	now := time.Now()
	g.now = func() time.Time { return now }
	ctx := context.Background()
	_, _ = g.JudgeNoul(ctx, ClaimQuestion, "a", claimQuestion)
	_, _ = g.JudgeNoul(ctx, ClaimQuestion, "b", claimQuestion)
	_, _ = g.JudgeNoul(ctx, ClaimQuestion, "a", claimQuestion)
	if f.calls.Load() != 3 {
		t.Fatalf("eviction: calls = %d, want 3", f.calls.Load())
	}
	now = now.Add(2 * time.Hour)
	_, _ = g.JudgeNoul(ctx, ClaimQuestion, "a", claimQuestion)
	if f.calls.Load() != 4 {
		t.Fatalf("expiry: calls = %d, want 4", f.calls.Load())
	}
}

func TestConcurrencyLimit(t *testing.T) {
	f := &fake{yes: 0.5, delay: 20 * time.Millisecond}
	c := cfg()
	c.MaxConcurrent = 2
	c.CacheEntries = 0
	g, _ := New(f, c, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g.JudgeNoul(context.Background(), ClaimQuestion, i, claimQuestion)
		}(i)
	}
	wg.Wait()
	if f.peak.Load() > 2 {
		t.Fatalf("peak = %d, want <= 2", f.peak.Load())
	}
}

func TestConfigValidate(t *testing.T) {
	bad := Config{Thresholds: map[string]Threshold{"q": {Accept: 0.2, Reject: 0.8}}}
	if _, err := New(&fake{}, bad, nil); err == nil {
		t.Fatal("expected error for reject >= accept")
	}
}

func TestEvaluateClaim(t *testing.T) {
	ctx := context.Background()
	f := &fake{yes: 0.99}
	g, _ := New(f, cfg(), nil)
	v, _, _ := g.EvaluateClaim(ctx, false, "the JSON is corrupted")
	if v != ClaimGenuine || f.calls.Load() != 0 {
		t.Fatalf("invalid input must not call the model: %v calls=%d", v, f.calls.Load())
	}
	if v, _, _ = g.EvaluateClaim(ctx, true, "the JSON is corrupted"); v != ClaimSpurious {
		t.Fatalf("got %v", v)
	}
	g2, _ := New(&fake{yes: 0.004}, cfg(), nil)
	if v, _, _ = g2.EvaluateClaim(ctx, true, `{"ok":true}`); v != ClaimNone {
		t.Fatalf("got %v", v)
	}
	g3, _ := New(&fake{yes: 0.5}, cfg(), nil)
	if v, _, _ = g3.EvaluateClaim(ctx, true, "hmm"); v != ClaimUnsure {
		t.Fatalf("got %v", v)
	}
	g4, _ := New(&fake{err: errors.New("down")}, cfg(), nil)
	if v, _, err := g4.EvaluateClaim(ctx, true, "x"); v != ClaimUnsure || err == nil {
		t.Fatalf("got %v %v", v, err)
	}
}

func TestRetryBudget(t *testing.T) {
	ctx := context.Background()
	g, _ := New(&fake{yes: 0.99}, cfg(), nil)
	n := 0
	_, v, err := RetryBudget{Max: 2}.Run(ctx, g, true, func(_ context.Context, a int, note string) (string, error) {
		n++
		if a > 0 && note == "" {
			t.Error("retry should carry a note")
		}
		return "corrupted", nil
	})
	if !errors.Is(err, ErrBudgetExhausted) || v != ClaimSpurious || n != 3 {
		t.Fatalf("v=%v err=%v n=%d", v, err, n)
	}
}
