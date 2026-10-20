package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQuotaResponseClassification(t *testing.T) {
	for _, status := range []int{403, 429, 503} {
		for _, header := range []string{"Retry-After", "X-RateLimit-Reset", "X-RateLimit-Remaining", ""} {
			t.Run(fmt.Sprintf("%d/%s", status, header), func(t *testing.T) {
				resp := &http.Response{StatusCode: status, Header: make(http.Header)}
				if header != "" {
					resp.Header.Set(header, "0")
				}
				_, limited := AsRateLimitError(newProviderResponseError(resp, "GET", "/test", nil))
				if want := status == 429 || header != ""; limited != want {
					t.Fatalf("limited=%v want=%v", limited, want)
				}
			})
		}
	}
}

func TestADOQuota503HonorsReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(10 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(503)
	}))
	defer server.Close()
	p := NewADOProvider("org", "project", "token", WithADOMaxRateLimitRetries(0), func(p *ADOProvider) {
		p.BaseURL = server.URL
		p.now = func() time.Time { return now }
		p.sleep = func(context.Context, time.Duration) error {
			t.Fatal("request must surrender quota wait to caller")
			return nil
		}
	})
	_, err := p.GetWorkItem(context.Background(), RepositoryRef{Project: "project"}, "1")
	limit, ok := AsRateLimitError(err)
	if !ok || !limit.Reset.Equal(reset) || limit.RetryDelay(now) != 10*time.Minute {
		t.Fatalf("quota=%+v recognized=%v err=%v", limit, ok, err)
	}
}

func TestGitHubResetHeaderClassifiesForbidden(t *testing.T) {
	resp := &http.Response{StatusCode: 403, Header: make(http.Header)}
	resp.Header.Set("X-RateLimit-Reset", "1792491606")
	if !isRateLimited(resp) {
		t.Fatal("reset-guided 403 was not classified as quota")
	}
}

func TestQuotaRetryDelayHonorsBothSignals(t *testing.T) {
	now := time.Now()
	limit := &RateLimitError{Reset: now.Add(time.Minute), RetryAfterRaw: "180"}
	if got := limit.RetryDelay(now); got != 3*time.Minute {
		t.Fatalf("delay=%v", got)
	}
}

func TestQuotaClassificationPreservesPermissionDeniedWithRemainingQuota(t *testing.T) {
	resp := &http.Response{StatusCode: 403, Header: make(http.Header)}
	resp.Header.Set("X-RateLimit-Remaining", "42")
	resp.Header.Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
	if _, limited := AsRateLimitError(newProviderResponseError(resp, "GET", "/test", nil)); limited {
		t.Fatal("permission error became quota wait")
	}
}

func TestSecondaryQuotaHonorsRetryAfterWithUnexhaustedPrimaryQuota(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	limit := &RateLimitError{Provider: ProviderGitHub, Secondary: true, RemainingKnown: true, Remaining: 42, Reset: now.Add(time.Hour), RetryAfterRaw: "120"}
	if got := limit.RetryDelay(now); got != 2*time.Minute {
		t.Fatalf("secondary wait=%v", got)
	}
	resp := &http.Response{StatusCode: 403, Header: make(http.Header)}
	resp.Header.Set("X-RateLimit-Remaining", "42")
	resp.Header.Set("X-RateLimit-Reset", fmt.Sprint(limit.Reset.Unix()))
	resp.Header.Set("Retry-After", "120")
	p := NewGitHubProvider("token")
	p.now = func() time.Time { return now }
	p.jitter = func(time.Duration) time.Duration { return 0 }
	wait, event := p.rateLimitPlan(resp, "GET /test", 0)
	if wait != 2*time.Minute+rateLimitResetSlack || !event.Secondary {
		t.Fatalf("wait=%v event=%+v", wait, event)
	}
}
