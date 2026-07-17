package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

type fakeBranchDeleter func(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error)

func (f fakeBranchDeleter) DeleteBranch(ctx context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
	return f(ctx, req)
}

func newTerminalBranchJournal(t *testing.T, pushed, openedPR bool) (string, string, *journal.Run) {
	t.Helper()
	const runID = "terminal-branch-run"
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:    runID,
		Workflow: "implementation",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	if err := jr.Append(journal.Event{
		Type: journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{
			Provider: "github",
			Kind:     "branch",
			ID:       providers.BranchName("implementation", runID),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if pushed {
		if err := jr.Append(journal.Event{
			Type:   journal.EventStageFinished,
			Stage:  "push-branch",
			Status: string(apiv1.ResultSuccess),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if openedPR {
		if err := jr.Append(journal.Event{
			Type:   journal.EventStageFinished,
			Stage:  "open-pr",
			Status: string(apiv1.ResultSuccess),
			Outputs: map[string]any{
				"prNumber": "42",
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return runsDir, runID, jr
}

func terminalBranchCleanupEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var cleanup []journal.Event
	for _, ev := range events {
		if ev.ExternalRef != nil && ev.ExternalRef.Kind == "branch" && ev.Runner["operation"] == branchCleanupOperation {
			cleanup = append(cleanup, ev)
		}
	}
	return cleanup
}

func TestFinalizeTerminalBranchDecisions(t *testing.T) {
	providerErr := errors.New("delete denied")
	tests := []struct {
		name         string
		pushed       bool
		openedPR     bool
		deleteResult providers.DeleteBranchResult
		deleteErr    error
		wantCalls    int
		wantOutcome  string
		wantReason   string
		wantError    bool
	}{
		{
			name:         "deletes pushed branch without pull request",
			pushed:       true,
			deleteResult: providers.DeleteBranchResult{Deleted: true},
			wantCalls:    1,
			wantOutcome:  branchCleanupSucceeded,
		},
		{
			name:        "never pushed",
			wantOutcome: branchCleanupSkipped,
			wantReason:  "branch-not-pushed",
		},
		{
			name:        "pull request opened",
			pushed:      true,
			openedPR:    true,
			wantOutcome: branchCleanupSkipped,
			wantReason:  "pull-request-opened",
		},
		{
			name:        "branch already absent",
			pushed:      true,
			wantCalls:   1,
			wantOutcome: branchCleanupSkipped,
			wantReason:  "branch-not-found",
		},
		{
			name:        "provider failure is journaled",
			pushed:      true,
			deleteErr:   providerErr,
			wantCalls:   1,
			wantOutcome: branchCleanupFailed,
			wantError:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runsDir, runID, jr := newTerminalBranchJournal(t, tc.pushed, tc.openedPR)
			var calls int
			deleteBranch := func(_ context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
				calls++
				if req.Name != providers.BranchName("implementation", runID) {
					t.Fatalf("branch = %q", req.Name)
				}
				return tc.deleteResult, tc.deleteErr
			}
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
			if err := finalizeTerminalBranch(runsDir, runID, jr, repo, deleteBranch); err != nil {
				t.Fatalf("finalizeTerminalBranch: %v", err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("delete calls = %d, want %d", calls, tc.wantCalls)
			}
			events := terminalBranchCleanupEvents(t, runsDir, runID)
			if len(events) != 1 {
				t.Fatalf("cleanup events = %d, want 1: %+v", len(events), events)
			}
			if got := events[0].Runner["outcome"]; got != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %q", got, tc.wantOutcome)
			}
			if got := events[0].Runner["reason"]; got != tc.wantReason && (got != nil || tc.wantReason != "") {
				t.Fatalf("reason = %v, want %q", got, tc.wantReason)
			}
			if got := events[0].Error != nil; got != tc.wantError {
				t.Fatalf("event error present = %t, want %t", got, tc.wantError)
			}
			if tc.wantError && events[0].Error.Code != "branch_delete_failed" {
				t.Fatalf("error = %+v", events[0].Error)
			}
		})
	}
}

func TestFinalizeTerminalBranchIsIdempotent(t *testing.T) {
	runsDir, runID, jr := newTerminalBranchJournal(t, true, false)
	var calls int
	deleteBranch := func(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
		calls++
		return providers.DeleteBranchResult{Deleted: true}, nil
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	for range 2 {
		if err := finalizeTerminalBranch(runsDir, runID, jr, repo, deleteBranch); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("delete calls = %d, want 1", calls)
	}
	if events := terminalBranchCleanupEvents(t, runsDir, runID); len(events) != 1 {
		t.Fatalf("cleanup events = %d, want 1", len(events))
	}
}

func TestRunAbortPreparesTerminalBranchCleanup(t *testing.T) {
	tests := []struct {
		name        string
		deleteErr   error
		wantOutcome string
	}{
		{name: "deletion succeeds", wantOutcome: branchCleanupSucceeded},
		{name: "provider failure preserves abort", deleteErr: errors.New("delete denied"), wantOutcome: branchCleanupFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOBERS_GITHUB_TOKEN", "ghp_abort_fixture_dummy_token")
			root := initDeterministicDemo(t)
			l := instance.NewLayout(root)
			const runID = "manually-aborted-run"

			jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
				RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "example",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := jr.Append(journal.Event{
				Type: journal.EventRefTouched,
				ExternalRef: &journal.ExternalRef{
					Provider: "github",
					Kind:     "branch",
					ID:       providers.BranchName("implementation", runID),
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := jr.Append(journal.Event{
				Type:   journal.EventStageFinished,
				Stage:  "push-branch",
				Status: string(apiv1.ResultSuccess),
			}); err != nil {
				t.Fatal(err)
			}
			if err := jr.Close(); err != nil {
				t.Fatal(err)
			}

			previous := newTerminalBranchDeleter
			var calls int
			newTerminalBranchDeleter = func(providers.TokenSource) providers.BranchDeleter {
				return fakeBranchDeleter(func(_ context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
					calls++
					if req.Name != providers.BranchName("implementation", runID) {
						t.Fatalf("branch = %q", req.Name)
					}
					return providers.DeleteBranchResult{Deleted: tc.deleteErr == nil}, tc.deleteErr
				})
			}
			t.Cleanup(func() { newTerminalBranchDeleter = previous })

			code, _, stderr := runArgs(t, "run", "abort", runID, root)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			if calls != 1 {
				t.Fatalf("delete calls = %d, want 1", calls)
			}

			events := terminalBranchCleanupEvents(t, l.RunsDir(), runID)
			if len(events) != 1 {
				t.Fatalf("cleanup events = %d, want 1: %+v", len(events), events)
			}
			if got := events[0].Runner["outcome"]; got != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %q", got, tc.wantOutcome)
			}
			if got := events[0].Error != nil; got != (tc.deleteErr != nil) {
				t.Fatalf("cleanup error present = %t, want %t", got, tc.deleteErr != nil)
			}

			rd, err := journal.OpenRead(filepath.Join(l.RunsDir(), runID))
			if err != nil {
				t.Fatal(err)
			}
			allEvents, err := rd.Events()
			if err != nil {
				t.Fatal(err)
			}
			if len(allEvents) < 2 {
				t.Fatalf("events = %+v", allEvents)
			}
			finished := allEvents[len(allEvents)-1]
			if finished.Type != journal.EventRunFinished || finished.Status != string(journal.PhaseAborted) {
				t.Fatalf("last event = %+v, want aborted run.finished", finished)
			}
			if events[0].Seq >= finished.Seq {
				t.Fatalf("cleanup seq = %d, run.finished seq = %d", events[0].Seq, finished.Seq)
			}
		})
	}
}

func TestBuildTerminalBranchDeleteAdmitsDedicatedCapability(t *testing.T) {
	t.Setenv("BRANCH_DELETE_TOKEN", "branch-delete-token")
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: "github",
		Owner:    "acme",
		Name:     "app",
		Token:    instance.TokenRef{Env: "BRANCH_DELETE_TOKEN"},
	}}}
	registrar := journal.NewRegistryScrubber()
	previous := newTerminalBranchDeleter
	var gotToken string
	var fail bool
	newTerminalBranchDeleter = func(source providers.TokenSource) providers.BranchDeleter {
		return fakeBranchDeleter(func(ctx context.Context, _ providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
			token, err := source.Token(ctx)
			if err != nil {
				return providers.DeleteBranchResult{}, err
			}
			gotToken = token
			if fail {
				return providers.DeleteBranchResult{}, errors.New("provider echoed " + token)
			}
			return providers.DeleteBranchResult{Deleted: true}, nil
		})
	}
	t.Cleanup(func() { newTerminalBranchDeleter = previous })

	deleteBranch, repo, err := buildTerminalBranchDelete(cfg, registrar)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Owner != "acme" || repo.Name != "app" {
		t.Fatalf("repo = %+v", repo)
	}
	result, err := deleteBranch(context.Background(), providers.DeleteBranchRequest{Repository: repo, Name: "goobers/implementation/run"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deleted || gotToken != "branch-delete-token" {
		t.Fatalf("result = %+v, token = %q", result, gotToken)
	}
	fail = true
	if _, err := deleteBranch(context.Background(), providers.DeleteBranchRequest{Repository: repo, Name: "goobers/implementation/run"}); err == nil {
		t.Fatal("expected provider error")
	} else if strings.Contains(err.Error(), "branch-delete-token") || !strings.Contains(err.Error(), journal.Redacted) {
		t.Fatalf("provider error was not scrubbed: %q", err)
	}
	if !capability.Known(string(capability.GitHubBranchDelete)) {
		t.Fatalf("branch-delete capability %q is not canonical", capability.GitHubBranchDelete)
	}
}

// TestBuildTerminalBranchPreparerSkipsCleanupWithoutARepo is issue #587's
// regression: an instance with no configured repo (the credential-free demo)
// runs workflows that never touch a branch by design — that absence is not
// the anomaly finalizeTerminalBranch's "branch-reference-missing" cleanup
// record exists to flag (a real repo-backed run whose branch ref.touched
// somehow never got journaled). buildTerminalBranchPreparer must skip branch
// cleanup entirely for a repo-less instance rather than journal a spurious
// ref.touched for every single terminal run.
func TestBuildTerminalBranchPreparerSkipsCleanupWithoutARepo(t *testing.T) {
	cfg := &instance.Config{}
	registrar := journal.NewRegistryScrubber()
	prepare, err := buildTerminalBranchPreparer(instance.Layout{}, cfg, registrar)
	if err != nil {
		t.Fatal(err)
	}

	const runID = "scratch-only-run"
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID, Workflow: "demo"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Close() })

	// No branch/push/PR events at all — exactly what a scratch-only demo
	// run's journal looks like.
	if err := prepare(runID, journal.PhaseCompleted, jr); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == journal.EventRefTouched {
			t.Fatalf("events = %+v, want no ref.touched appended for a repo-less instance", events)
		}
	}
}
