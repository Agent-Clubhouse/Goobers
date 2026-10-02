package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type trackingResponseBody struct {
	io.Reader
	closed bool
}

func (b *trackingResponseBody) Close() error {
	b.closed = true
	return nil
}

type bodyStateObserver struct {
	body                *trackingResponseBody
	observed            bool
	bodyClosedAtObserve bool
}

func (o *bodyStateObserver) ObserveRateLimit(context.Context, RateLimitEvent) {
	o.observed = true
	o.bodyClosedAtObserve = o.body.closed
}

func TestGiteaRateLimitExhaustionReturnsFinalResponse(t *testing.T) {
	body := &trackingResponseBody{Reader: strings.NewReader("rate limited")}
	client := newProviderHTTPClient(time.Second)
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})
	provider := NewGiteaProvider("https://gitea.example.com", "token",
		WithGiteaHTTPClient(client),
		WithGiteaMaxRateLimitRetries(0),
	)

	resp, err := provider.send(context.Background(), http.MethodGet, provider.BaseURL+"/repos/acme/app", nil)
	if err != nil {
		t.Fatalf("send() error = %v, want final response", err)
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("send() response = %#v, want final 429 response", resp)
	}
	if body.closed {
		t.Fatal("exhausted 429 body was closed before being returned to the caller")
	}
	_ = resp.Body.Close()
}

func TestADORateLimitExhaustionReturnsFinalResponse(t *testing.T) {
	body := &trackingResponseBody{Reader: strings.NewReader("rate limited")}
	client := newProviderHTTPClient(time.Second)
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})
	provider := NewADOProvider("org", "project", "token",
		func(p *ADOProvider) { p.Client = client },
		WithADOMaxRateLimitRetries(0),
	)

	resp, err := provider.send(context.Background(), http.MethodGet, "https://ado.example/x", nil, "")
	if err != nil {
		t.Fatalf("send() error = %v, want final response", err)
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("send() response = %#v, want final 429 response", resp)
	}
	if body.closed {
		t.Fatal("exhausted 429 body was closed before being returned to the caller")
	}
	_ = resp.Body.Close()
}

func TestGitHubRateLimitExhaustionReturnsTypedError(t *testing.T) {
	body := &trackingResponseBody{Reader: strings.NewReader("rate limited")}
	observer := &bodyStateObserver{body: body}
	client := newProviderHTTPClient(time.Second)
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})
	provider := NewGitHubProvider("token",
		WithHTTPClient(client),
		WithMaxRateLimitRetries(0),
		WithRateLimitObserver(observer),
	)

	resp, err := provider.send(context.Background(), http.MethodGet, "https://api.github.example/repos/acme/app", nil)
	var rateLimitErr *RateLimitError
	if resp != nil || !errors.As(err, &rateLimitErr) {
		t.Fatalf("send() = (%#v, %v), want nil response and *RateLimitError", resp, err)
	}
	if !body.closed {
		t.Fatal("exhausted GitHub 429 body was not closed")
	}
	if !observer.observed || !observer.bodyClosedAtObserve {
		t.Fatalf("rate-limit observer = {observed:%t bodyClosed:%t}, want body closed before observation",
			observer.observed, observer.bodyClosedAtObserve)
	}
}

func TestGiteaRetryClosesResponseBodies(t *testing.T) {
	testRetryClosesResponseBodies(t, func(client HTTPClient) (*http.Response, error) {
		provider := NewGiteaProvider("https://gitea.example.com", "token",
			WithGiteaHTTPClient(client),
			WithGiteaMaxTransientRetries(1),
			WithGiteaMaxRateLimitRetries(1),
		)
		provider.sleep = func(context.Context, time.Duration) error { return nil }
		return provider.send(context.Background(), http.MethodGet, provider.BaseURL+"/repos/acme/app", nil)
	})
}

func TestGitHubRetryClosesResponseBodies(t *testing.T) {
	testRetryClosesResponseBodies(t, func(client HTTPClient) (*http.Response, error) {
		provider := NewGitHubProvider("token",
			WithHTTPClient(client),
			WithMaxTransientRetries(1),
			WithMaxRateLimitRetries(1),
		)
		provider.sleep = func(context.Context, time.Duration) error { return nil }
		return provider.send(context.Background(), http.MethodGet, "https://api.github.example/repos/acme/app", nil)
	})
}

func testRetryClosesResponseBodies(t *testing.T, send func(HTTPClient) (*http.Response, error)) {
	t.Helper()
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			retriedBody := &trackingResponseBody{Reader: strings.NewReader("retry")}
			finalBody := &trackingResponseBody{Reader: strings.NewReader("{}")}
			attempt := 0
			client := newProviderHTTPClient(time.Second)
			client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempt++
				if attempt == 1 {
					return &http.Response{
						StatusCode: status,
						Header:     make(http.Header),
						Body:       retriedBody,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       finalBody,
				}, nil
			})

			resp, err := send(client)
			if err != nil {
				t.Fatalf("send() error = %v", err)
			}
			if attempt != 2 {
				t.Fatalf("attempts = %d, want 2", attempt)
			}
			if !retriedBody.closed {
				t.Fatal("retried response body was not closed")
			}
			if finalBody.closed {
				t.Fatal("final response body was closed before being returned")
			}
			_ = resp.Body.Close()
		})
	}
}
