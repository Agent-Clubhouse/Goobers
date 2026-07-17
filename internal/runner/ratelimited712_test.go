package runner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// TestRunnerTaskRateLimitedNotifiesHandler is #712's runner-side acceptance:
// a stage failing with providers.ErrorCodeRateLimited and a parseable
// rateLimitReset output notifies Config.RateLimited with the parsed reset
// time, before the run's ordinary failure-routing decision — regardless of
// what that decision turns out to be (here: PhaseFailed, since
// terminalFailMachine's "implement" task has no gate Next).
func TestRunnerTaskRateLimitedNotifiesHandler(t *testing.T) {
	machine := terminalFailMachine(t)
	resetAt := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	byTask := map[string]stubTaskResult{
		"run-ratelimited:implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: providers.ErrorCodeRateLimited, Message: "list work items: github rate limited", Retryable: true},
			outputs:   map[string]interface{}{"rateLimitReset": resetAt.Format(time.RFC3339)},
		},
	}
	r, _ := newTestRunner(t, byTask, nil)

	var got RateLimitedOutcome
	var called bool
	r.cfg.RateLimited = func(_ context.Context, o RateLimitedOutcome) error {
		called = true
		got = o
		return nil
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-ratelimited",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed (RateLimited is a side notification, not a routing change)", res.Phase)
	}
	if !called {
		t.Fatal("expected Config.RateLimited to be called")
	}
	want := RateLimitedOutcome{RunID: "run-ratelimited", Stage: "implement", ResetAt: resetAt}
	if got != want {
		t.Fatalf("RateLimitedOutcome = %+v, want %+v", got, want)
	}
}

// TestRunnerTaskRateLimitedWithoutParseableResetSkipsHandler proves the
// handler is only called when a reset time was actually recovered — a
// github_rate_limited failure with no (or unparseable) rateLimitReset output
// carries nothing actionable for the scheduler, so notifying would just
// teach it a zero-value "exhausted forever" fact (RecordExhausted's own
// zero-is-noop guard would no-op it anyway, but taskOutcome skips the call
// entirely rather than relying on that).
func TestRunnerTaskRateLimitedWithoutParseableResetSkipsHandler(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-ratelimited-noreset:implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: providers.ErrorCodeRateLimited, Message: "rate limited, no reset header"},
		},
	}
	r, _ := newTestRunner(t, byTask, nil)

	var called bool
	r.cfg.RateLimited = func(context.Context, RateLimitedOutcome) error {
		called = true
		return nil
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-ratelimited-noreset",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if called {
		t.Fatal("expected Config.RateLimited NOT to be called without a parseable reset")
	}
}

// TestRunnerTaskNonRateLimitedFailureSkipsHandler proves the hook is scoped
// to providers.ErrorCodeRateLimited specifically — an ordinary business
// failure (even with an incidentally-similarly-shaped output) must not
// trigger the provider-quota notification.
func TestRunnerTaskNonRateLimitedFailureSkipsHandler(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-ordinary-fail:implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"},
			outputs:   map[string]interface{}{"rateLimitReset": time.Now().Add(time.Hour).Format(time.RFC3339)},
		},
	}
	r, _ := newTestRunner(t, byTask, nil)

	var called bool
	r.cfg.RateLimited = func(context.Context, RateLimitedOutcome) error {
		called = true
		return nil
	}

	if _, err := r.Start(context.Background(), StartInput{
		RunID:   "run-ordinary-fail",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if called {
		t.Fatal("expected Config.RateLimited NOT to be called for a non-rate-limited failure code")
	}
}

// TestRunnerTaskRateLimitedHandlerErrorStillTerminal proves the RateLimited
// handler is best-effort like Blocked: its own failure is journaled
// (rate_limited_handling_failed) via failTerminal, but never silently lost.
func TestRunnerTaskRateLimitedHandlerErrorStillTerminal(t *testing.T) {
	machine := terminalFailMachine(t)
	resetAt := time.Now().Add(time.Hour)
	byTask := map[string]stubTaskResult{
		"run-ratelimited-herr:implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: providers.ErrorCodeRateLimited, Message: "rate limited"},
			outputs:   map[string]interface{}{"rateLimitReset": resetAt.Format(time.RFC3339)},
		},
	}
	r, runsDir := newTestRunner(t, byTask, nil)
	r.cfg.RateLimited = func(context.Context, RateLimitedOutcome) error {
		return errors.New("scheduler state unreachable")
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-ratelimited-herr",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed even though the handler errored", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-ratelimited-herr"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawHandlerFailure bool
	for _, e := range events {
		if e.Type == journal.EventError && e.Error != nil && e.Error.Code == "rate_limited_handling_failed" {
			sawHandlerFailure = true
		}
	}
	if !sawHandlerFailure {
		t.Fatal("expected a rate_limited_handling_failed error event recording the handler's own failure")
	}
}
