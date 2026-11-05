package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestCIPollQuotaWaitPreservesFailureBudget(t *testing.T) {
	for _, provider := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(provider), func(t *testing.T) {
			now := time.Now()
			steps := []pollStep{{err: errors.New("status 503: unavailable")}}
			for i := 0; i < 9; i++ {
				steps = append(steps, pollStep{err: &providers.RateLimitError{
					Provider: provider, Status: 429, RetryAfterRaw: "120",
				}})
			}
			steps = append(steps, pollStep{err: errors.New("status 503: unavailable")}, pollStep{state: providers.CheckStatePassing})
			rec := newFakeRecorder()
			e, err := NewCIPollExecutor(&sequencedPoller{steps: steps}, rec)
			if err != nil {
				t.Fatal(err)
			}
			e.MaxConsecutivePollErrors = 2
			e.Now = func() time.Time { return now }
			e.Jitter = func(time.Duration) time.Duration { return 0 }
			e.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
			result, err := e.Run(context.Background(), CIPollConfig{Owner: "o", Repo: "r", PullID: "1", Timeout: time.Hour})
			if err != nil || result.Status != apiv1.ResultSuccess {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if len(rec.events) != 9 {
				t.Fatalf("quota events=%d, want 9", len(rec.events))
			}
			for _, event := range rec.events {
				if event.Runner["kind"] != "ci_poll_rate_limit_wait" || event.Runner["consecutiveErrors"] != 1 {
					t.Fatalf("quota consumed or cleared prior failure budget: %+v", event)
				}
			}
		})
	}
}

func TestCIPollQuotaResetBeyondTimeout(t *testing.T) {
	for _, provider := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		t.Run(string(provider), func(t *testing.T) {
			now := time.Now()
			start := now
			poller := &alwaysErrorPoller{err: &providers.RateLimitError{Provider: provider, Reset: now.Add(time.Hour)}}
			e, err := NewCIPollExecutor(poller, newFakeRecorder())
			if err != nil {
				t.Fatal(err)
			}
			e.Now = func() time.Time { return now }
			e.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
			result, err := e.Run(context.Background(), CIPollConfig{Owner: "o", Repo: "r", PullID: "1", Timeout: 5 * time.Minute, CarryOutputs: map[string]any{"kept": "yes"}})
			if err != nil || result.Error == nil || result.Error.Code != "poll_timeout" || result.Outputs[OutputCIStatus] != CIStatusTimeout {
				t.Fatalf("result=%+v err=%v, want timeout without infrastructure error", result, err)
			}
			if now.Sub(start) != 5*time.Minute || poller.calls != 1 || result.Outputs["kept"] != "yes" {
				t.Fatalf("elapsed=%v calls=%d outputs=%v", now.Sub(start), poller.calls, result.Outputs)
			}
		})
	}
}

func TestCIPollQuotaCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e, err := NewCIPollExecutor(&alwaysErrorPoller{err: &providers.RateLimitError{Secondary: true}}, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	e.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	_, err = e.Run(ctx, cfgFor("o", "r", "1"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestCIPollSecondaryQuotaWait(t *testing.T) {
	for _, raw := range []string{"", "180"} {
		t.Run(raw, func(t *testing.T) {
			now := time.Now()
			start := now
			e, err := NewCIPollExecutor(&sequencedPoller{steps: []pollStep{
				{err: &providers.RateLimitError{Provider: providers.ProviderGitHub, Secondary: true, RetryAfterRaw: raw}},
				{state: providers.CheckStatePassing},
			}}, newFakeRecorder())
			if err != nil {
				t.Fatal(err)
			}
			e.Now = func() time.Time { return now }
			e.Jitter = func(time.Duration) time.Duration { return time.Second }
			e.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
			result, err := e.Run(context.Background(), cfgFor("o", "r", "1"))
			want := 63 * time.Second
			if raw != "" {
				want = 183 * time.Second
			}
			if err != nil || result.Status != apiv1.ResultSuccess || now.Sub(start) != want {
				t.Fatalf("elapsed=%v want=%v result=%+v err=%v", now.Sub(start), want, result, err)
			}
		})
	}
}

func TestCIPollQuotaWaitHonorsEarlierStageDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	poller := &alwaysErrorPoller{err: &providers.RateLimitError{Reset: time.Now().Add(time.Hour)}}
	e, err := NewCIPollExecutor(poller, newFakeRecorder())
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Run(ctx, CIPollConfig{Owner: "o", Repo: "r", PullID: "1", Timeout: time.Hour})
	if err != nil || result.Error == nil || result.Error.Code != "poll_timeout" || poller.calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, poller.calls)
	}
}
