package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGitHubSendDoesNotRetryTransportErrorOnPost is #2026's core regression:
// a POST whose response is lost to a transport error may have already
// committed server-side (issue creation, a comment post, a label
// mutation) — a blind retry would duplicate it. Mirrors
// TestADOSendDoesNotRetryTransportErrorOnPost.
func TestGitHubSendDoesNotRetryTransportErrorOnPost(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	client := newProviderHTTPClient(time.Second)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return nil, errTransientTestNetwork
	})

	provider := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.Client = client
		p.BaseURL = "https://api.github.example"
		p.sleep = func(context.Context, time.Duration) error {
			t.Fatal("sleep() called — a non-idempotent method must not be retried")
			return nil
		}
	})

	_, err := provider.send(context.Background(), http.MethodPost, "https://api.github.example/x", nil)
	if err == nil {
		t.Fatal("send() error = nil, want the transport error surfaced immediately")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (no retry on POST)", attempts)
	}
}

// TestGitHubSendRetries5xxOnIdempotentMethodOnly mirrors
// TestADOSendRetries5xxOnIdempotentMethodOnly: a 5xx can follow a request
// that already committed server-side, so retry stays restricted to
// idempotent methods.
func TestGitHubSendRetries5xxOnIdempotentMethodOnly(t *testing.T) {
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
				n := attempts
				mu.Unlock()
				if n == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("{}"))
			}))
			defer server.Close()

			provider := NewGitHubProvider("token", func(p *GitHubProvider) {
				p.BaseURL = server.URL
				p.sleep = func(context.Context, time.Duration) error { return nil }
			})

			resp, err := provider.send(context.Background(), test.method, server.URL+"/x", nil)
			if test.wantRetries {
				if err != nil {
					t.Fatalf("send(): %v, want the retried request to eventually succeed", err)
				}
				_ = resp.Body.Close()
				if attempts != 2 {
					t.Fatalf("attempts = %d, want 2 (initial 500 + one retry)", attempts)
				}
				return
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d, want exactly 1 (no retry on a non-idempotent method's 5xx)", attempts)
			}
		})
	}
}

// TestGitHubGraphQLQueryRetriesTransportErrorDespitePostWireMethod pins the
// subtlety #2026's fix has to get right: GraphQL's wire method is always
// POST, but a query (read) is exactly as safe to retry as a REST GET —
// isGraphQLMutation, not the literal HTTP method, must drive the
// retry-safety decision here.
func TestGitHubGraphQLQueryRetriesTransportErrorDespitePostWireMethod(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	client := newProviderHTTPClient(time.Second)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			return nil, errTransientTestNetwork
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":{}}`)),
			Header:     make(http.Header),
		}, nil
	})

	provider := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.Client = client
		p.BaseURL = "https://api.github.example"
		p.sleep = func(context.Context, time.Duration) error { return nil }
	})

	err := provider.graphql(context.Background(), `query($x:Int!){ viewer { login } }`, map[string]interface{}{"x": 1}, nil)
	if err != nil {
		t.Fatalf("graphql(): %v, want the retried query to eventually succeed", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (initial transport error + one retry, despite the POST wire method)", attempts)
	}
}

// TestGitHubGraphQLMutationDoesNotRetryTransportError is the mirror case: a
// mutation (write) must not be retried blindly, exactly like a REST POST.
func TestGitHubGraphQLMutationDoesNotRetryTransportError(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	client := newProviderHTTPClient(time.Second)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return nil, errTransientTestNetwork
	})

	provider := NewGitHubProvider("token", func(p *GitHubProvider) {
		p.Client = client
		p.BaseURL = "https://api.github.example"
		p.sleep = func(context.Context, time.Duration) error {
			t.Fatal("sleep() called — a mutation must not be retried")
			return nil
		}
	})

	err := provider.graphql(context.Background(), `mutation($x:ID!){ enqueuePullRequest(input:{pullRequestId:$x}){ clientMutationId } }`, map[string]interface{}{"x": "PR_1"}, nil)
	if err == nil {
		t.Fatal("graphql() error = nil, want the transport error surfaced immediately")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (no retry on a mutation)", attempts)
	}
}
