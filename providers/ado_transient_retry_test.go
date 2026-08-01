package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestADOSendRetriesTransportErrorOnIdempotentMethod(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	client := newProviderHTTPClient(time.Second)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		if attempt < 3 {
			return nil, errTransientTestNetwork
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
		}, nil
	})

	var waits []time.Duration
	provider := NewADOProvider("org", "project", "ado-secret",
		func(p *ADOProvider) {
			p.Client = client
			p.BaseURL = "https://ado.example"
			p.sleep = func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return nil
			}
		},
		WithADOMaxRateLimitRetries(5),
	)

	resp, err := provider.send(context.Background(), http.MethodGet, "https://ado.example/x", nil, "")
	if err != nil {
		t.Fatalf("send() error = %v, want a successful retry", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (2 failures + 1 success)", attempts)
	}
	if len(waits) != 2 {
		t.Fatalf("recorded backoff waits = %v, want 2 entries", waits)
	}
	if waits[0] != backoffDuration(0) || waits[1] != backoffDuration(1) {
		t.Fatalf("backoff waits = %v, want [%s, %s] (GET's bounded backoff curve)", waits, backoffDuration(0), backoffDuration(1))
	}
}

func TestADOSendDoesNotRetryTransportErrorOnPost(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	client := newProviderHTTPClient(time.Second)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		// Simulate the response being lost after the server already committed
		// the POST — a blind retry here would duplicate the mutation (#2026).
		return nil, errTransientTestNetwork
	})

	provider := NewADOProvider("org", "project", "ado-secret", func(p *ADOProvider) {
		p.Client = client
		p.BaseURL = "https://ado.example"
		p.sleep = func(context.Context, time.Duration) error {
			t.Fatal("sleep() called — a non-idempotent method must not be retried")
			return nil
		}
	})

	_, err := provider.send(context.Background(), http.MethodPost, "https://ado.example/x", nil, "")
	if err == nil {
		t.Fatal("send() error = nil, want the transport error surfaced immediately")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (no retry on POST)", attempts)
	}
}

func TestADOSendRetries5xxOnIdempotentMethodOnly(t *testing.T) {
	for _, test := range []struct {
		name        string
		method      string
		wantRetries bool
	}{
		{name: "GET retries", method: http.MethodGet, wantRetries: true},
		{name: "PUT retries", method: http.MethodPut, wantRetries: true},
		{name: "DELETE retries", method: http.MethodDelete, wantRetries: true},
		{name: "POST does not retry", method: http.MethodPost, wantRetries: false},
		{name: "PATCH does not retry", method: http.MethodPatch, wantRetries: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts++
				attempt := attempts
				mu.Unlock()
				if attempt < 2 {
					http.Error(w, "server error", http.StatusBadGateway)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			provider := NewADOProvider("org", "project", "ado-secret", func(p *ADOProvider) {
				p.BaseURL = server.URL
				p.sleep = func(context.Context, time.Duration) error { return nil }
			})

			resp, err := provider.send(context.Background(), test.method, server.URL+"/x", nil, "")
			if err != nil {
				t.Fatalf("send() error = %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if test.wantRetries {
				if attempts != 2 || resp.StatusCode != http.StatusOK {
					t.Fatalf("attempts = %d, status = %d, want a retried request that recovers to 200", attempts, resp.StatusCode)
				}
			} else {
				if attempts != 1 || resp.StatusCode != http.StatusBadGateway {
					t.Fatalf("attempts = %d, status = %d, want the 502 returned immediately with no retry", attempts, resp.StatusCode)
				}
			}
		})
	}
}

func TestADOSendBoundsTransientRetriesByMaxRetries(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	client := newProviderHTTPClient(time.Second)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return nil, errTransientTestNetwork
	})

	const retries = 2
	provider := NewADOProvider("org", "project", "ado-secret",
		func(p *ADOProvider) {
			p.Client = client
			p.BaseURL = "https://ado.example"
			p.sleep = func(context.Context, time.Duration) error { return nil }
		},
		WithADOMaxRateLimitRetries(retries),
	)

	_, err := provider.send(context.Background(), http.MethodGet, "https://ado.example/x", nil, "")
	if err == nil {
		t.Fatal("send() error = nil, want the transport error surfaced once retries are exhausted")
	}
	if attempts != retries+1 {
		t.Fatalf("attempts = %d, want %d (initial + %d retries)", attempts, retries+1, retries)
	}
}

// errTransientTestNetwork simulates a transport-level failure (connection
// reset, DNS blip) distinct from an HTTP error response.
var errTransientTestNetwork error = fakeTransportError{}

type fakeTransportError struct{}

func (fakeTransportError) Error() string { return "simulated transport error" }
