package providers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestIsTransientError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"5xx status", fmt.Errorf("GET /pulls/1 failed: status 503: service unavailable"), true},
		{"rate limited status", fmt.Errorf("GET /pulls/1 failed: status 429: rate limited"), true},
		{"4xx not found", fmt.Errorf("GET /pulls/1 failed: status 404: not found"), false},
		{"4xx forbidden", fmt.Errorf("GET /pulls/1 failed: status 403: forbidden"), false},
		{"wrapped net error", fmt.Errorf("send request: %w", &net.DNSError{Err: "temporary failure", IsTemporary: true}), true},
		{"typed rate-limit give-up", &RateLimitError{Endpoint: "/repos/acme/app/issues", Status: 403}, true},
		{"wrapped typed rate-limit give-up", fmt.Errorf("list work items: %w", &RateLimitError{Endpoint: "/x", Status: 429}), true},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped context canceled", fmt.Errorf("send request: %w", context.Canceled), false},
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
