package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// failedHandlerDispositionFixture seeds a repo-backed instance with one claim
// and returns the wired Failed handler plus the fake poster it writes through.
func failedHandlerDispositionFixture(t *testing.T, runID string) (runner.FailedHandler, *blockedHandlerFakeCommenter) {
	t.Helper()
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("2458", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})
	if h == nil {
		t.Fatal("expected a non-nil handler for a repo-backed instance")
	}
	return h, fake
}

// TestFailedHandlerSkipsInfraAndItemJudgmentDispositions is #3364's interim
// ask against #3361/#3363's vocabulary: the failure-streak circuit breaker
// counts WORK failures. A run that died because credential materialization
// failed (the live 2026-08-20 GET /user 403) tells us nothing about the item,
// and a run that ended because the implementer correctly refused a stale issue
// tells us the machine was RIGHT — neither may accumulate strikes that
// eventually park the item goobers:needs-human.
//
// Even at the threshold count, an excluded disposition posts nothing at all:
// no streak comment, no labels.
func TestFailedHandlerSkipsInfraAndItemJudgmentDispositions(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{"credential materialization fault", telemetry.ErrCodeCredentialUnavailable},
		{"git provisioning fault", telemetry.ErrCodeInfraGit},
		{"network fault", telemetry.ErrCodeInfraNet},
		{"claims-lock contention", telemetry.ErrCodeClaimsLock},
		{"verified item refusal", telemetry.ErrCodeIssueNotApplicable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const runID = "run-infra"
			h, fake := failedHandlerDispositionFixture(t, runID)
			for i := 0; i < failureStreakThreshold; i++ {
				if err := h(context.Background(), runner.FailedOutcome{
					RunID:   runID,
					RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
					Stage:   "implement",
					Code:    tc.code,
					Cause:   tc.code + ": the substrate failed, not the work",
				}); err != nil {
					t.Fatalf("handler call %d: %v", i+1, err)
				}
			}
			if len(fake.calls) != 0 {
				t.Fatalf("provider calls = %+v, want none — %q is not a work failure and must not accrue a failure streak",
					fake.calls, tc.code)
			}
		})
	}
}

// TestFailedHandlerStillCountsWorkFailures is the control: the exclusion is
// keyed on the terminal's disposition class, not applied blanket. A timeout
// (this circuit breaker's own motivating case, #1054) and an untyped
// walk-level failure both still count.
func TestFailedHandlerStillCountsWorkFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{"harness session timeout", telemetry.ErrCodeTimeout},
		{"untyped walk-level failure", "run_failed"},
		{"stage business failure", "nonzero_exit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const runID = "run-work"
			h, fake := failedHandlerDispositionFixture(t, runID)
			if err := h(context.Background(), runner.FailedOutcome{
				RunID:   runID,
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
				Stage:   "implement",
				Code:    tc.code,
			}); err != nil {
				t.Fatalf("handler: %v", err)
			}
			if len(fake.calls) != 1 {
				t.Fatalf("provider calls = %d, want 1 (a work failure still leaves its countable trace)", len(fake.calls))
			}
		})
	}
}

func TestFailedHandlerUsesCachedStreakWithoutCommentListCall(t *testing.T) {
	const runID = "run-cached-streak"
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("2458", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	if err := writeFailureStreakState(l, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}, "2458", 2, "run-older", "implement"); err != nil {
		t.Fatalf("write cached streak: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}}}}
	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})
	if err := h(context.Background(), runner.FailedOutcome{
		RunID:   runID,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage:   "implement",
		Code:    telemetry.ErrCodeTimeout,
	}); err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(fake.calls) != 2 {
		t.Fatalf("provider calls = %d, want 2 (cached count increments once and reaches the threshold label update)", len(fake.calls))
	}
	if got, err := loadFailureStreakState(l, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}, "2458"); err != nil || got != 3 {
		t.Fatalf("persisted streak = %d, %v; want 3", got, err)
	}
}

func TestFailedHandlerDoesNotCountWhenStreakReadIsRateLimited(t *testing.T) {
	const runID = "run-rate-limited"
	fake := &blockedHandlerFakeCommenter{listErr: &providers.RateLimitError{Provider: providers.ProviderGitHub, Endpoint: "/comments", Status: 403, Remaining: 0, Reset: time.Now().Add(time.Hour)}}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("2459", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}}}}
	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})
	if err := h(context.Background(), runner.FailedOutcome{
		RunID:   runID,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage:   "implement",
		Code:    telemetry.ErrCodeTimeout,
	}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("provider calls = %d, want 0 when the streak count itself is rate limited", len(fake.calls))
	}
	if got, err := loadFailureStreakState(l, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}, "2459"); err == nil && got != 0 {
		t.Fatalf("streak count = %d, want 0 when the count call was rate limited and not incremented", got)
	}
}
