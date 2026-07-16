package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGitHubRateLimitWaitsUntilReset is #614's backoff acceptance: a primary
// rate limit whose X-RateLimit-Reset is beyond the old blanket 60s cap (but
// within the wait budget) must sleep until the reset actually passes and then
// succeed — the old capped wait could never straddle the window, so every
// retry burned against a still-zero quota.
func TestGitHubRateLimitWaitsUntilReset(t *testing.T) {
	fixed := time.Unix(1_784_200_000, 0)
	reset := fixed.Add(90 * time.Second)
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":123,"number":7,"title":"ok","state":"open"}`))
	}))
	defer srv.Close()

	var waits []time.Duration
	p := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.BaseURL = srv.URL
		p.now = func() time.Time { return fixed }
		p.sleep = func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		}
	})

	item, err := p.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err != nil {
		t.Fatalf("GetWorkItem after reset-length backoff: %v", err)
	}
	if item.Title != "ok" {
		t.Fatalf("expected success after waiting out the reset, got %#v", item)
	}
	if len(waits) != 1 {
		t.Fatalf("expected exactly one reset-length sleep, got %v", waits)
	}
	if waits[0] < 90*time.Second {
		t.Fatalf("wait %v did not reach the 90s-out reset (old 60s cap resurfaced?)", waits[0])
	}
}

// TestGitHubRateLimitResetBeyondBudgetFailsFastTyped is #614's detection
// acceptance: a 403 whose reset is further out than the wait budget must not
// sleep at all — it returns a typed *RateLimitError (code github_rate_limited,
// reset time attached) immediately, instead of the generic "status 403"
// string the non-2xx path used to fold it into.
func TestGitHubRateLimitResetBeyondBudgetFailsFastTyped(t *testing.T) {
	fixed := time.Unix(1_784_200_000, 0)
	reset := fixed.Add(30 * time.Minute)
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	var waits []time.Duration
	p := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.BaseURL = srv.URL
		p.now = func() time.Time { return fixed }
		p.sleep = func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		}
	})

	_, err := p.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v (%T), want *RateLimitError", err, err)
	}
	if !rl.Reset.Equal(time.Unix(reset.Unix(), 0)) {
		t.Fatalf("Reset = %v, want %v", rl.Reset, time.Unix(reset.Unix(), 0))
	}
	if rl.Status != http.StatusForbidden || rl.Remaining != 0 {
		t.Fatalf("typed error carries status %d / remaining %d, want 403 / 0", rl.Status, rl.Remaining)
	}
	if !strings.Contains(err.Error(), ErrorCodeRateLimited) {
		t.Fatalf("error message %q does not carry the %s code", err.Error(), ErrorCodeRateLimited)
	}
	if len(waits) != 0 {
		t.Fatalf("expected fail-fast with no sleeps toward an unreachable reset, got %v", waits)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("expected a single request before failing fast, got %d", n)
	}
	if !IsTransientError(err) {
		t.Fatal("typed rate-limit error must classify as transient (quota resets on the clock)")
	}
}
