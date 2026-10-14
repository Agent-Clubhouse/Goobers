package harness

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/credentials"
)

// rateLimitedSignIn is the Copilot CLI's sign-in failure while the provider
// is rate limiting the account (#6017): it carries "login" and "403", which
// the credential markers alone read as a missing sign-in.
const rateLimitedSignIn = `Error: Failed to fetch PAT user login (403): {"message":"API rate limit exceeded for user ID 1.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`

func TestClassifyTransientProbeOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		output     string
		transient  bool
		retryAfter time.Duration
	}{
		{"primary rate limit message", rateLimitedSignIn, true, 0},
		{"exhausted quota header", "Failed to fetch PAT user login (403)\nx-ratelimit-remaining: 0", true, 0},
		{"secondary rate limit retry-after", "Failed to fetch PAT user login (403): forbidden\nRetry-After: 120", true, 2 * time.Minute},
		{"json retry-after", `request failed {"status":403,"headers":{"retry-after":"45"}}`, true, 45 * time.Second},
		{"too many requests", "Request failed with status code 429", true, 0},
		{"bad gateway", "Failed to fetch PAT user login (502): Bad Gateway", true, 0},
		{"service unavailable", "error: 503 Service Unavailable", true, 0},
		{"dns failure", "TypeError: fetch failed: getaddrinfo ENOTFOUND api.github.com", true, 0},
		{"connection reset", "read ECONNRESET", true, 0},
		{"absurd retry-after is capped", "Retry-After: 99999999999999999999", true, MaxTransientPreflightRetryAfter},

		{"plain 403 is a sign-in failure", "Failed to fetch PAT user login (403): Resource not accessible by personal access token", false, 0},
		{"401 wins over a rate-limit mention", "Failed to fetch PAT user login (401): Bad credentials; API rate limit exceeded", false, 0},
		{"signed out", "Error: not signed in. Run copilot login", false, 0},
		{"unrelated failure", "unknown flag: --available-tools", false, 0},
		{"500 tokens is not a status", "replied with 500 tokens of output", false, 0},
		{"empty", "", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transient, retryAfter := classifyTransientProbeOutput([]byte(tc.output))
			if transient != tc.transient || retryAfter != tc.retryAfter {
				t.Fatalf("classify(%q) = (%t, %s), want (%t, %s)", tc.output, transient, retryAfter, tc.transient, tc.retryAfter)
			}
		})
	}
}

// TestCopilotPreflightClassifiesRateLimitedSignInAsTransient is #6017 end to
// end through the adapter: the rate-limited sign-in probe used to surface as
// HARNESS_AUTH_REQUIRED ("run goobers harness auth copilot login"), which is
// wrong advice and an ordinary preflight failure. It is now a transient
// provider failure that carries the provider's Retry-After.
func TestCopilotPreflightClassifiesRateLimitedSignInAsTransient(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		output     string
		retryAfter time.Duration
	}{
		{"rate limit message", rateLimitedSignIn, 0},
		{"retry-after", rateLimitedSignIn + "\nretry-after: 90", 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeProcessRunner{result: ProcessResult{Transcript: []byte("copilot version 1.2.3\n")}}
			runner.act = func(req ProcessRequest) error {
				if slices.Contains(req.Command, "-p") {
					runner.result = ProcessResult{ExitCode: 1, Transcript: []byte(tc.output)}
				}
				return nil
			}
			adapter := &CopilotAdapter{
				Command:            []string{program},
				AuthCheckArgs:      []string{"-p", "Reply with exactly: ok"},
				AuthProbeExtraArgs: []string{"--no-color"},
				Runner:             runner,
			}
			_, err := adapter.Preflight(context.Background())
			if err == nil {
				t.Fatal("expected the rate-limited sign-in probe to fail preflight")
			}
			if !errors.Is(err, credentials.ErrTransientProvider) {
				t.Fatalf("err = %v, want a transient provider failure", err)
			}
			if IsHarnessAuthRequired(err) {
				t.Fatalf("err = %v, a rate-limited provider is not a missing sign-in", err)
			}
			if strings.Contains(err.Error(), "may no longer accept these flags") {
				t.Fatalf("err = %v, blames the configured probe flags for a provider rate limit", err)
			}
			if !strings.Contains(err.Error(), "API rate limit exceeded") || !strings.Contains(err.Error(), "retried with backoff") {
				t.Fatalf("err = %v, want the provider response and the retry remediation", err)
			}
			got, ok := TransientPreflightRetryAfter(err)
			if tc.retryAfter == 0 {
				if ok {
					t.Fatalf("retry-after = %s, want none", got)
				}
				return
			}
			if !ok || got != tc.retryAfter {
				t.Fatalf("retry-after = %s (%t), want %s", got, ok, tc.retryAfter)
			}
		})
	}
}

// TestCopilotLauncherProbeDoesNotReprobeARateLimitedProvider: a forwarding
// launcher's probe is retried once on a bootstrap failure, but a provider
// that is rate limiting must not be asked again immediately (#6017).
func TestCopilotLauncherProbeDoesNotReprobeARateLimitedProvider(t *testing.T) {
	result := ProcessResult{ExitCode: 1, Transcript: []byte("session bootstrap: upstream 503 Service Unavailable")}
	if shouldRetryCopilotLauncherAuthProbe(context.Background(), result, nil, true) {
		t.Fatal("a provider 503 was treated as a launcher bootstrap failure and re-probed immediately")
	}
	err := copilotAuthProbeError(context.Background(), "launcher authentication probe", result, nil, true)
	if !errors.Is(err, credentials.ErrTransientProvider) {
		t.Fatalf("err = %v, want a transient provider failure", err)
	}
}

func TestCopilotAuthProbeErrorKeepsTimeoutAheadOfTransientOutput(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	err := copilotAuthProbeError(ctx, "sign-in check", ProcessResult{ExitCode: 1, Transcript: []byte(rateLimitedSignIn)}, ErrTimeout, false)
	if !errors.Is(err, ErrTimeout) || errors.Is(err, credentials.ErrTransientProvider) {
		t.Fatalf("err = %v, want the probe timeout reported as such", err)
	}
}

func TestClaudePreflightClassifiesTransientAuthStatus(t *testing.T) {
	runner := &claudeSequenceRunner{results: []ProcessResult{
		{Transcript: []byte("Claude Code test version")},
		{ExitCode: 1, Transcript: []byte("API Error: 529 overloaded_error; status 529")},
	}}
	adapter := &ClaudeAdapter{Command: []string{"echo"}, Runner: runner}
	_, err := adapter.Preflight(context.Background())
	if !errors.Is(err, credentials.ErrTransientProvider) {
		t.Fatalf("err = %v, want a transient provider failure", err)
	}
	runner = &claudeSequenceRunner{results: []ProcessResult{
		{Transcript: []byte("Claude Code test version")},
		{ExitCode: 1, Transcript: []byte(`{"loggedIn":false}`)},
	}}
	adapter = &ClaudeAdapter{Command: []string{"echo"}, Runner: runner}
	if _, err := adapter.Preflight(context.Background()); err == nil || errors.Is(err, credentials.ErrTransientProvider) {
		t.Fatalf("err = %v, want a signed-out Claude to stay an ordinary preflight failure", err)
	}
}
