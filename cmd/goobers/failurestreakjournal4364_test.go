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

// TestFailureStreakPersistsInJournalAcrossHandlerCalls is #4364's core
// regression: the streak count survives across separate buildFailedHandler
// invocations (as separate runs would see it) without ever calling
// ListComments to reconstruct it — only UpsertFailureComment's own
// find-the-existing-comment lookup touches the provider, and that call
// succeeds every time here, so a failing/rate-limited COUNT call (the bug)
// has nothing left to corrupt.
func TestFailureStreakPersistsInJournalAcrossHandlerCalls(t *testing.T) {
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
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

	for i, runID := range []string{"run-1", "run-2", "run-3"} {
		if ok, _, err := ledger.Claim("2701", runID, "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("seed claim %d: ok=%v err=%v", i, ok, err)
		}
		h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})
		if err := h(context.Background(), runner.FailedOutcome{
			RunID:   runID,
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
			Stage:   "implement",
			Code:    telemetry.ErrCodeTimeout,
		}); err != nil {
			t.Fatalf("handler call %d: %v", i, err)
		}
		if got, err := loadFailureStreakCount(l, repo, "2701"); err != nil || got != i+1 {
			t.Fatalf("streak after call %d = %d, %v; want %d", i, got, err, i+1)
		}
		if err := ledger.Release("2701", runID); err != nil {
			t.Fatalf("release claim %d: %v", i, err)
		}
	}

	labelMutations := 0
	for _, call := range fake.calls {
		if len(call.AddLabels) > 0 {
			labelMutations++
		}
	}
	if labelMutations != 1 {
		t.Fatalf("label-mutation calls = %d, want 1 (circuit breaker trips once, at the threshold)", labelMutations)
	}
}

// TestFailureStreakNotAdvancedWhenCommentWriteRateLimited is #4364's second
// acceptance criterion: a rate-limited write while trying to record a failure
// must not advance the cached streak — "I couldn't post" is not evidence the
// item failed again.
func TestFailureStreakNotAdvancedWhenCommentWriteRateLimited(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{listErr: &providers.RateLimitError{
		Provider: providers.ProviderGitHub, Endpoint: "/comments", Status: 403, Remaining: 0, Reset: time.Now().Add(time.Hour),
	}}
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
	if ok, _, err := ledger.Claim("2702", "run-rate-limited", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

	h := buildFailedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})
	if err := h(context.Background(), runner.FailedOutcome{
		RunID:   "run-rate-limited",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage:   "implement",
		Code:    telemetry.ErrCodeTimeout,
	}); err == nil {
		t.Fatal("handler error = nil, want a surfaced rate-limit failure")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("label-mutation calls = %d, want 0 (never reaches the threshold check)", len(fake.calls))
	}
	if got, err := loadFailureStreakCount(l, repo, "2702"); err != nil || got != 0 {
		t.Fatalf("streak = %d, %v; want 0 (rate-limited comment write must not advance the cache)", got, err)
	}
}
