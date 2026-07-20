package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestADORateLimitHonorsRetryAfter(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			w.Header().Set("Retry-After", "3")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		writeJSON(t, w, map[string]any{
			"id": 42,
			"fields": map[string]any{
				"System.WorkItemType": "Issue",
				"System.Title":        "recovered",
				"System.State":        "Active",
			},
		})
	}))
	defer server.Close()

	observer := &recordingObserver{}
	provider := NewADOProvider("org", "project", "ado-secret",
		func(p *ADOProvider) { p.BaseURL = server.URL },
		WithADORateLimitObserver(observer),
	)
	var waits []time.Duration
	provider.sleep = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	item, err := provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project"}, "42")
	if err != nil {
		t.Fatalf("GetWorkItem() error = %v", err)
	}
	if item.Title != "recovered" {
		t.Fatalf("GetWorkItem() title = %q, want recovered", item.Title)
	}
	if len(waits) != 1 || waits[0] < 3*time.Second {
		t.Fatalf("rate-limit waits = %v, want one wait honoring Retry-After=3", waits)
	}
	if observer.count() != 1 {
		t.Fatalf("rate-limit events = %d, want 1", observer.count())
	}
	event := observer.events[0]
	if event.Provider != ProviderADO || event.Outcome != RateLimitOutcomeRetry {
		t.Fatalf("rate-limit event = %#v", event)
	}
	if event.RetryAfter != 3*time.Second || event.Delay != waits[0] {
		t.Fatalf("event delay = %s (Retry-After %s), waits = %v", event.Delay, event.RetryAfter, waits)
	}
	if strings.Contains(event.Scope, "?") || !strings.HasSuffix(event.Scope, "/org/project/_apis/wit/workitems/42") {
		t.Fatalf("event scope = %q, want credential-safe endpoint scope", event.Scope)
	}
}

func TestRateLimitFallbackIsJitteredAndBounded(t *testing.T) {
	for _, test := range []struct {
		name   string
		jitter func(time.Duration) time.Duration
		want   time.Duration
	}{
		{name: "lower bound", jitter: func(time.Duration) time.Duration { return 0 }, want: 500 * time.Millisecond},
		{name: "upper bound", jitter: func(max time.Duration) time.Duration { return max }, want: time.Second},
		{name: "clamps oversized jitter", jitter: func(time.Duration) time.Duration { return time.Hour }, want: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := fallbackBackoff(0, test.jitter); got != test.want {
				t.Fatalf("fallbackBackoff(0) = %s, want %s", got, test.want)
			}
		})
	}
	if got := fallbackBackoff(100, func(max time.Duration) time.Duration { return max }); got != rateLimitBackoffMax {
		t.Fatalf("large-attempt fallback = %s, want cap %s", got, rateLimitBackoffMax)
	}
}

func TestADORateLimitUsesFallbackWithoutServerDelay(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		writeJSON(t, w, map[string]any{
			"id": 42,
			"fields": map[string]any{
				"System.WorkItemType": "Issue",
				"System.Title":        "recovered",
				"System.State":        "Active",
			},
		})
	}))
	defer server.Close()

	observer := &recordingObserver{}
	provider := NewADOProvider("org", "project", "token",
		func(p *ADOProvider) { p.BaseURL = server.URL },
		WithADORateLimitObserver(observer),
	)
	provider.jitter = func(max time.Duration) time.Duration { return max }
	var wait time.Duration
	provider.sleep = func(_ context.Context, delay time.Duration) error {
		wait = delay
		return nil
	}

	if _, err := provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project"}, "42"); err != nil {
		t.Fatalf("GetWorkItem() error = %v", err)
	}
	if wait != time.Second {
		t.Fatalf("fallback wait = %s, want bounded attempt-0 ceiling 1s", wait)
	}
	if event := observer.events[0]; event.RetryAfter != 0 || event.Delay != wait || event.Outcome != RateLimitOutcomeRetry {
		t.Fatalf("fallback event = %#v", event)
	}
}

func TestADORateLimitExhaustionPreservesResponseError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	observer := &recordingObserver{}
	provider := NewADOProvider("org", "project", "token",
		func(p *ADOProvider) { p.BaseURL = server.URL },
		WithADOMaxRateLimitRetries(0),
		WithADORateLimitObserver(observer),
	)
	_, err := provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project"}, "42")
	if err == nil || !strings.Contains(err.Error(), "failed: status 429: rate limited") {
		t.Fatalf("GetWorkItem() error = %v, want existing provider response error", err)
	}
	if observer.count() != 1 || observer.events[0].Outcome != RateLimitOutcomeExhausted {
		t.Fatalf("exhausted rate-limit events = %#v", observer.events)
	}
}

func TestADOCommitRetriesRateLimitedPathPreflight(t *testing.T) {
	itemCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/items"):
			itemCalls++
			if itemCalls == 1 {
				w.Header().Set("Retry-After", "2")
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/pushes"):
			writeJSON(t, w, map[string]any{
				"url":     "push-url",
				"commits": []map[string]string{{"commitId": "commit-sha"}},
			})
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	observer := &recordingObserver{}
	provider := NewADOProvider("org", "project", "token",
		func(p *ADOProvider) { p.BaseURL = server.URL },
		WithADORateLimitObserver(observer),
	)
	var waits []time.Duration
	provider.sleep = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	commit, err := provider.Commit(context.Background(), CommitRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		Branch:     "work",
		BaseSHA:    "base-sha",
		Message:    "update docs",
		Files:      []CommitFile{{Path: "README.md", Content: "updated"}},
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if commit.SHA != "commit-sha" {
		t.Fatalf("Commit() SHA = %q, want commit-sha", commit.SHA)
	}
	if itemCalls != 2 {
		t.Fatalf("path preflight calls = %d, want 2", itemCalls)
	}
	if len(waits) != 1 || waits[0] < 2*time.Second {
		t.Fatalf("path preflight waits = %v, want one wait honoring Retry-After=2", waits)
	}
	if observer.count() != 1 || observer.events[0].Outcome != RateLimitOutcomeRetry {
		t.Fatalf("path preflight rate-limit events = %#v", observer.events)
	}
}

func TestGitHubSecondaryRateLimitWithoutHeadersUsesFallback(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, `{"message":"You have exceeded a secondary rate limit."}`, http.StatusForbidden)
			return
		}
		writeJSON(t, w, map[string]any{"id": 123, "number": 7, "title": "recovered", "state": "open"})
	}))
	defer server.Close()

	observer := &recordingObserver{}
	provider := NewGitHubProvider("token",
		func(p *GitHubProvider) { p.BaseURL = server.URL },
		WithRateLimitObserver(observer),
	)
	provider.jitter = func(time.Duration) time.Duration { return 0 }
	var wait time.Duration
	provider.sleep = func(_ context.Context, delay time.Duration) error {
		wait = delay
		return nil
	}

	item, err := provider.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err != nil {
		t.Fatalf("GetWorkItem() error = %v", err)
	}
	if item.Title != "recovered" {
		t.Fatalf("GetWorkItem() title = %q, want recovered", item.Title)
	}
	if wait < time.Minute || wait > time.Minute+rateLimitBackoffBase {
		t.Fatalf("secondary fallback wait = %s, want bounded jitter above one minute", wait)
	}
	event := observer.events[0]
	if !event.Secondary || event.Outcome != RateLimitOutcomeRetry || event.Delay != wait {
		t.Fatalf("secondary fallback event = %#v", event)
	}
}

func TestGitHubOrdinaryForbiddenResponseIsNotRetried(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	provider.sleep = func(context.Context, time.Duration) error {
		t.Fatal("ordinary forbidden response unexpectedly retried")
		return nil
	}
	_, err := provider.GetWorkItem(context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, "7")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("GetWorkItem() error = %v, want original forbidden response", err)
	}
	if calls != 1 {
		t.Fatalf("ordinary forbidden request count = %d, want 1", calls)
	}
}

func TestRateLimitRetryDoesNotBlockUnrelatedOperation(t *testing.T) {
	var mu sync.Mutex
	limitedCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/1") {
			mu.Lock()
			limitedCalls++
			call := limitedCalls
			mu.Unlock()
			if call == 1 {
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
		}
		id := 1
		title := "limited"
		if strings.HasSuffix(r.URL.Path, "/2") {
			id = 2
			title = "unrelated"
		}
		writeJSON(t, w, map[string]any{
			"id": id,
			"fields": map[string]any{
				"System.WorkItemType": "Issue",
				"System.Title":        title,
				"System.State":        "Active",
			},
		})
	}))
	defer server.Close()

	sleepStarted := make(chan struct{})
	releaseSleep := make(chan struct{})
	sleepReleased := false
	defer func() {
		if !sleepReleased {
			close(releaseSleep)
		}
	}()
	var once sync.Once
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	provider.sleep = func(ctx context.Context, _ time.Duration) error {
		once.Do(func() { close(sleepStarted) })
		select {
		case <-releaseSleep:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	limitedDone := make(chan error, 1)
	go func() {
		_, err := provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project"}, "1")
		limitedDone <- err
	}()
	<-sleepStarted

	unrelatedDone := make(chan error, 1)
	go func() {
		item, err := provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project"}, "2")
		if err == nil && item.Title != "unrelated" {
			t.Errorf("unrelated item title = %q, want unrelated", item.Title)
		}
		unrelatedDone <- err
	}()
	select {
	case err := <-unrelatedDone:
		if err != nil {
			t.Fatalf("unrelated operation failed while another request backed off: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated operation was blocked by another request's backoff")
	}

	close(releaseSleep)
	sleepReleased = true
	if err := <-limitedDone; err != nil {
		t.Fatalf("limited operation did not recover: %v", err)
	}
}

func TestRetryAfterDelaySupportsHTTPDate(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	at := now.Add(4 * time.Second).Format(http.TimeFormat)
	if got, ok := retryAfterDelay(at, now); !ok || got != 4*time.Second {
		t.Fatalf("retryAfterDelay(%q) = %s, %v; want 4s, true", at, got, ok)
	}
	if _, ok := retryAfterDelay("not-a-delay", now); ok {
		t.Fatal("malformed Retry-After unexpectedly parsed")
	}
}

func TestRateLimitEventJSONOmitsEndpointAndRawHeaders(t *testing.T) {
	const credential = "credential-canary"
	data, err := json.Marshal(RateLimitEvent{
		Provider:      ProviderGitHub,
		Scope:         "api.github.com/repos/acme/app/issues",
		Delay:         time.Second,
		Outcome:       RateLimitOutcomeRetry,
		Endpoint:      "https://" + credential + "@api.github.com/issues?token=" + credential,
		RetryAfterRaw: credential,
		RemainingRaw:  credential,
		ResetRaw:      credential,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), credential) {
		t.Fatalf("rate-limit event leaked credential-bearing transport metadata: %s", data)
	}
	for _, field := range []string{`"provider":"github"`, `"scope":`, `"delay":`, `"outcome":"retry"`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("rate-limit event %s missing %s", data, field)
		}
	}
}
