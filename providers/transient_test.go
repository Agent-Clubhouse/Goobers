package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
)

func TestIsTransientError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"github 503 status", fmt.Errorf("GET /pulls/1 failed: status 503: service unavailable"), true},
		{"ado 502 status", fmt.Errorf("PATCH https://dev.azure.com/acme failed: status 502: bad gateway"), true},
		{"rate limited status", fmt.Errorf("GET /pulls/1 failed: status 429: rate limited; retry after 60s"), true},
		{"401 authentication", fmt.Errorf("GET /pulls/1 failed: status 401: bad credentials"), false},
		{"403 authorization", fmt.Errorf("GET /pulls/1 failed: status 403: forbidden"), false},
		{"403 rate limit response", fmt.Errorf("GET /pulls/1 failed: status 403: secondary rate limit; retry after 60s"), true},
		{"403 rate limit reset", fmt.Errorf(`GET /pulls/1 failed: status 403: exhausted (X-RateLimit-Remaining="0", X-RateLimit-Reset="1784210000")`), true},
		{"403 unguided rate limit", fmt.Errorf("GET /pulls/1 failed: status 403: rate limit exceeded"), false},
		{"404 not found", fmt.Errorf("GET /pulls/1 failed: status 404: not found"), false},
		{"422 deterministic request", fmt.Errorf("POST /pulls failed: status 422: validation failed"), false},
		{"wrapped dns error", fmt.Errorf("send request: %w", &net.DNSError{Err: "temporary failure", IsTemporary: true}), true},
		{"typed rate-limit give-up", &RateLimitError{Endpoint: "/repos/acme/app/issues", Status: 403}, true},
		{"wrapped typed rate-limit give-up", fmt.Errorf("list work items: %w", &RateLimitError{Endpoint: "/x", Status: 429}), true},
		{"serialized typed rate-limit give-up", errors.New((&RateLimitError{Endpoint: "/x", Status: 403, Secondary: true}).Error()), true},
		{"url client timeout", &url.Error{Op: "Get", URL: "https://api.github.com", Err: context.DeadlineExceeded}, true},
		{"url transport eof", &url.Error{Op: "Get", URL: "https://api.github.com", Err: io.EOF}, true},
		{"subprocess connection reset", errors.New("error: list work items: send request: read tcp: connection reset by peer"), true},
		{"subprocess transport eof", errors.New(`error: list work items: send request: Get "https://api.github.com/issues": EOF`), true},
		{"subprocess dns failure", errors.New("error: dial tcp: lookup api.github.com: no such host"), true},
		{"subprocess tls timeout", errors.New("error: net/http: TLS handshake timeout"), true},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped context canceled", fmt.Errorf("send request: %w", context.Canceled), false},
		{"malformed url", errors.New(`parse "://bad": missing protocol scheme`), false},
		{"tls certificate failure", errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), false},
		{"wrapped tls certificate failure", &url.Error{Op: "Get", URL: "https://api.github.com", Err: errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")}, false},
		{"wrapped deterministic url error", &url.Error{Op: "Get", URL: "://bad", Err: errors.New("unsupported protocol scheme")}, false},
		{"decode eof", fmt.Errorf("decode provider response: %w", io.EOF), false},
		{"opaque unrelated error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientError(tc.err); got != tc.want {
				t.Fatalf("IsTransientError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
