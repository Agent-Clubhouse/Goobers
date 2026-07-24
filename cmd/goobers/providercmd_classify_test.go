package main

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

// TestClassifyProviderError_RateLimitPrimary proves the pre-existing #614
// path is preserved unchanged: a primary rate-limit give-up classifies as
// providers.ErrorCodeRateLimited, retryable, with the reset time carried
// through as an extra field.
func TestClassifyProviderError_RateLimitPrimary(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute)
	err := &providers.RateLimitError{Endpoint: "/issues", Status: 403, Remaining: 0, Reset: reset}

	code, retryable, extra := classifyProviderError(err)
	if code != providers.ErrorCodeRateLimited {
		t.Fatalf("code = %q, want %q", code, providers.ErrorCodeRateLimited)
	}
	if !retryable {
		t.Fatal("retryable = false, want true")
	}
	if got := extra["rateLimitReset"]; got != reset.UTC().Format(time.RFC3339) {
		t.Fatalf("rateLimitReset = %v, want %v", got, reset.UTC().Format(time.RFC3339))
	}
}

// TestClassifyProviderError_RateLimitSecondary is #711's AC: a
// Retry-After-driven secondary/abuse limit must classify distinctly from a
// primary quota exhaustion, so an operator (and a future retry policy) can
// tell "back off briefly" from "wait for the hourly window".
func TestClassifyProviderError_RateLimitSecondary(t *testing.T) {
	err := &providers.RateLimitError{Endpoint: "/issues", Status: 403, Secondary: true}

	code, retryable, _ := classifyProviderError(err)
	if code != errorCodeSecondaryRateLimited {
		t.Fatalf("code = %q, want %q", code, errorCodeSecondaryRateLimited)
	}
	if !retryable {
		t.Fatal("retryable = false, want true — a secondary limit resolves on its own clock")
	}
	if code == providers.ErrorCodeRateLimited {
		t.Fatal("secondary limit must not collapse into the primary code")
	}
}

// TestClassifyProviderError_ServerError is #711's core acceptance: a 503 (or
// any 5xx) — the live-incident "Unicorn!" HTML page — classifies as
// github_server_error, retryable. providers' own non-2xx typed error
// (providerResponseError, #613) is unexported, so classifyProviderError
// recovers the status from the message text — this constructs the same
// "status %d" shape providers/http.go's newProviderResponseError formats
// into, the realistic wire shape rather than the type directly.
func TestClassifyProviderError_ServerError(t *testing.T) {
	err := errors.New("GET /issues failed: status 503: <html>Unicorn!</html>")

	code, retryable, extra := classifyProviderError(err)
	if code != errorCodeServerError {
		t.Fatalf("code = %q, want %q", code, errorCodeServerError)
	}
	if !retryable {
		t.Fatal("retryable = false, want true — 5xx is transient server-side load shedding")
	}
	if extra != nil {
		t.Fatalf("extra = %v, want nil for a server error", extra)
	}
}

// TestClassifyProviderError_AuthFailed401 and _403 prove #711's auth
// classification: a genuine permission failure (never itself a rate limit —
// isRateLimited already intercepts and reports those separately) must be
// github_auth_failed and non-retryable, since retrying with the same bad or
// expired credential cannot succeed.
func TestClassifyProviderError_AuthFailed401(t *testing.T) {
	err := errors.New("GET /issues failed: status 401: Bad credentials")

	code, retryable, _ := classifyProviderError(err)
	if code != errorCodeAuthFailed {
		t.Fatalf("code = %q, want %q", code, errorCodeAuthFailed)
	}
	if retryable {
		t.Fatal("retryable = true, want false — retrying a 401 with the same credential cannot succeed")
	}
}

func TestClassifyProviderError_AuthFailed403(t *testing.T) {
	// A plain 403 (not a *RateLimitError) can only occur when isRateLimited
	// already ruled this response out as a rate limit — i.e. a genuine
	// permission 403, not a quota one.
	err := errors.New("POST /issues/1/labels failed: status 403: Resource not accessible by integration")

	code, retryable, _ := classifyProviderError(err)
	if code != errorCodeAuthFailed {
		t.Fatalf("code = %q, want %q", code, errorCodeAuthFailed)
	}
	if retryable {
		t.Fatal("retryable = true, want false")
	}
}

// TestClassifyProviderError_UnclassifiedStatusFallsBackToProviderError
// covers a status this classification scheme doesn't give a dedicated code
// to (e.g. a 422 validation error) — the issue's design section calls this
// the "fallback provider_error carrying the underlying message" case.
func TestClassifyProviderError_UnclassifiedStatusFallsBackToProviderError(t *testing.T) {
	err := errors.New("POST /pulls failed: status 422: Validation Failed")

	code, retryable, _ := classifyProviderError(err)
	if code != errorCodeProvider {
		t.Fatalf("code = %q, want %q", code, errorCodeProvider)
	}
	if retryable {
		t.Fatal("retryable = true, want false for an unclassified 4xx")
	}
}

// TestClassifyProviderError_Network is #711's AC: a transport-level failure
// (dial/DNS/reset/timeout) that exhausted send()'s in-request retry budget
// classifies as network_error and retryable — the network blip is unrelated
// to the request's content. Mirrors send()'s actual wrapping
// (fmt.Errorf("send request: %w", err)) and relies on
// providers.IsTransientError's own network-error recognition (a *net.
// DNSError's "no such host" text) to decide retryable, so this classifier
// never disagrees with #613's own opinion.
func TestClassifyProviderError_Network(t *testing.T) {
	underlying := &net.DNSError{Err: "no such host", Name: "api.github.com", IsNotFound: true}
	wrapped := fmt.Errorf("send request: %w", underlying)

	code, retryable, _ := classifyProviderError(wrapped)
	if code != errorCodeNetwork {
		t.Fatalf("code = %q, want %q", code, errorCodeNetwork)
	}
	if !retryable {
		t.Fatal("retryable = false, want true")
	}
}

func TestClassifyProviderError_MergeQueuedBranch(t *testing.T) {
	err := errors.New("exit status 1: remote: error: GH006: Protected branch update failed\n" +
		"remote: - A pull request for this branch has been added to a merge queue.\n" +
		"remote: Branches that are queued for merging cannot be updated.")

	code, retryable, extra := classifyProviderError(err)
	if code != errorCodeBranchMergeQueued {
		t.Fatalf("code = %q, want %q", code, errorCodeBranchMergeQueued)
	}
	if !retryable {
		t.Fatal("retryable = false, want true while the merge queue owns the branch")
	}
	if extra != nil {
		t.Fatalf("extra = %v, want nil", extra)
	}
}

func TestClassifyProviderError_OtherProtectedBranchRejectionsStayTerminal(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "different GH006 policy",
			err:  errors.New("remote: error: GH006: Protected branch update failed\nremote: Changes must be made through a pull request."),
		},
		{
			name: "merge queue wording without GH006",
			err:  errors.New("branch was added to a merge queue"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, retryable, _ := classifyProviderError(tt.err)
			if code != errorCodeProvider {
				t.Fatalf("code = %q, want %q", code, errorCodeProvider)
			}
			if retryable {
				t.Fatal("retryable = true, want false")
			}
		})
	}
}

// TestClassifyProviderError_UnknownErrorFallsBackToProviderError proves the
// classifier never panics or leaves code empty for a plain, untyped error —
// every failProviderStage caller passes a provider-originated error, so this
// is the safety-net path, still typed and diagnosable rather than silently
// falling through to a bare "1" exit with no result file at all.
func TestClassifyProviderError_UnknownErrorFallsBackToProviderError(t *testing.T) {
	code, retryable, extra := classifyProviderError(errors.New("something unexpected"))
	if code != errorCodeProvider {
		t.Fatalf("code = %q, want %q", code, errorCodeProvider)
	}
	if retryable {
		t.Fatal("retryable = true, want false for an unrecognized error")
	}
	if extra != nil {
		t.Fatalf("extra = %v, want nil", extra)
	}
}
