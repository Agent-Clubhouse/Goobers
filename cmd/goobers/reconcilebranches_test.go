package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

type fakeBranchReconcileProvider struct {
	branches      []providers.BranchSummary
	listErr       error
	openPRs       map[string]providers.PullRequestResult
	lookupErrs    map[string]error
	current       map[string]providers.BranchSummary
	getErrs       map[string]error
	deleteResults map[string]providers.DeleteBranchResult
	deleteErrs    map[string]error

	listRequests   []providers.ListBranchesRequest
	lookupBranches []string
	getBranches    []string
	deleteBranches []string
}

func (f *fakeBranchReconcileProvider) ListBranches(_ context.Context, req providers.ListBranchesRequest) ([]providers.BranchSummary, error) {
	f.listRequests = append(f.listRequests, req)
	return f.branches, f.listErr
}

func (f *fakeBranchReconcileProvider) GetBranch(_ context.Context, _ providers.RepositoryRef, name string) (providers.BranchSummary, bool, error) {
	f.getBranches = append(f.getBranches, name)
	if err := f.getErrs[name]; err != nil {
		return providers.BranchSummary{}, false, err
	}
	if branch, ok := f.current[name]; ok {
		return branch, true, nil
	}
	for _, branch := range f.branches {
		if branch.Name == name {
			return branch, true, nil
		}
	}
	return providers.BranchSummary{}, false, nil
}

func (f *fakeBranchReconcileProvider) FindPullRequestByBranch(_ context.Context, _ providers.RepositoryRef, head, base string) (providers.PullRequestResult, bool, error) {
	if base != "" {
		return providers.PullRequestResult{}, false, errors.New("branch reconciliation must search every base")
	}
	f.lookupBranches = append(f.lookupBranches, head)
	if err := f.lookupErrs[head]; err != nil {
		return providers.PullRequestResult{}, false, err
	}
	pr, ok := f.openPRs[head]
	return pr, ok, nil
}

func (f *fakeBranchReconcileProvider) DeleteBranch(_ context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
	f.deleteBranches = append(f.deleteBranches, req.Name)
	if err := f.deleteErrs[req.Name]; err != nil {
		return providers.DeleteBranchResult{}, err
	}
	return f.deleteResults[req.Name], nil
}

func createBranchReconcileRun(t *testing.T, runsDir, workflow, runID string, started time.Time, terminal, recordBranch bool) string {
	t.Helper()
	return createBranchReconcileRunWithTerminalTime(t, runsDir, workflow, runID, started, started, terminal, recordBranch)
}

func createBranchReconcileRunWithTerminalTime(t *testing.T, runsDir, workflow, runID string, started, terminalAt time.Time, terminal, recordBranch bool) string {
	t.Helper()
	branch := providers.BranchName(workflow, runID)
	eventTime := started
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: workflow, StartedAt: started,
	}, nil, journal.WithClock(func() time.Time { return eventTime }))
	if err != nil {
		t.Fatal(err)
	}
	if recordBranch {
		if err := run.Append(journal.Event{
			Type: journal.EventRefTouched,
			ExternalRef: &journal.ExternalRef{
				Provider: string(providers.ProviderGitHub),
				Kind:     "branch",
				ID:       branch,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if terminal {
		eventTime = terminalAt
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return branch
}

func openBranchReconcileLog(t *testing.T) (string, *journal.InstanceLog) {
	t.Helper()
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return dir, log
}

func branchReconcileEvents(t *testing.T, dir string) []journal.Event {
	t.Helper()
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var reconciled []journal.Event
	for _, event := range events {
		if event.Runner["operation"] == branchReconcileOperation {
			reconciled = append(reconciled, event)
		}
	}
	return reconciled
}

func TestReconcileRemoteBranchesDryRunNeverDeletes(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runsDir := t.TempDir()
	branch := createBranchReconcileRun(t, runsDir, "implementation", "old-run", now.Add(-8*24*time.Hour), true, true)
	logDir, log := openBranchReconcileLog(t)
	provider := &fakeBranchReconcileProvider{
		branches:      []providers.BranchSummary{{Name: branch, SHA: "old-sha"}},
		deleteResults: map[string]providers.DeleteBranchResult{branch: {Deleted: true}},
	}

	report, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    runsDir,
		Prefix:     branchReconcilePrefix,
		Limit:      25,
		MinimumAge: 7 * 24 * time.Hour,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("reconcileRemoteBranches: %v", err)
	}
	if report.Scanned != 1 || report.Candidates != 1 || report.Preserved != 1 || report.Deleted != 0 {
		t.Fatalf("report = %+v", report)
	}
	if len(provider.deleteBranches) != 0 {
		t.Fatalf("delete calls = %v, want none", provider.deleteBranches)
	}
	events := branchReconcileEvents(t, logDir)
	if len(events) != 1 || events[0].Runner["event"] != "decision" ||
		events[0].Runner["outcome"] != "candidate" || events[0].Runner["reason"] != "dry-run" {
		t.Fatalf("events = %+v", events)
	}
}

func TestReconcileRemoteBranchesUsesTerminalTimeForSafetyWindow(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runsDir := t.TempDir()
	branch := createBranchReconcileRunWithTerminalTime(
		t,
		runsDir,
		"implementation",
		"recently-finished",
		now.Add(-30*24*time.Hour),
		now.Add(-time.Hour),
		true,
		true,
	)
	logDir, log := openBranchReconcileLog(t)
	provider := &fakeBranchReconcileProvider{
		branches: []providers.BranchSummary{{Name: branch, SHA: "recent-sha"}},
	}

	report, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    runsDir,
		Prefix:     branchReconcilePrefix,
		Limit:      25,
		MinimumAge: 7 * 24 * time.Hour,
		Delete:     true,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("reconcileRemoteBranches: %v", err)
	}
	if report.Scanned != 1 || report.Candidates != 0 || report.Preserved != 1 || report.Deleted != 0 {
		t.Fatalf("report = %+v", report)
	}
	if len(provider.lookupBranches) != 0 || len(provider.getBranches) != 0 || len(provider.deleteBranches) != 0 {
		t.Fatalf("provider calls: PR=%v get=%v delete=%v", provider.lookupBranches, provider.getBranches, provider.deleteBranches)
	}
	events := branchReconcileEvents(t, logDir)
	if len(events) != 1 || events[0].Runner["reason"] != "safety-window" ||
		events[0].Runner["terminalAt"] != now.Add(-time.Hour).Format(time.RFC3339) {
		t.Fatalf("events = %+v", events)
	}
}

func TestReconcileBranchesCommandDefaultsToDryRun(t *testing.T) {
	now := time.Now().UTC()
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	branch := createBranchReconcileRun(t, layout.RunsDir(), "implementation", "cli-dry-run", now.Add(-8*24*time.Hour), true, true)
	var deleteCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/git/matching-refs/heads/goobers":
			if err := json.NewEncoder(w).Encode([]map[string]any{{
				"ref": "refs/heads/" + branch, "object": map[string]string{"sha": "branch-sha"},
			}}); err != nil {
				t.Fatal(err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/pulls":
			if _, present := r.URL.Query()["base"]; present {
				t.Fatalf("pull lookup unexpectedly restricted to base: %s", r.URL.RawQuery)
			}
			if got := r.URL.Query().Get("head"); got != "your-org:"+branch {
				t.Fatalf("pull lookup head = %q", got)
			}
			if err := json.NewEncoder(w).Encode([]map[string]any{}); err != nil {
				t.Fatal(err)
			}
		case r.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected provider request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousProvider := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		opts = append(opts, func(provider *providers.GitHubProvider) { provider.BaseURL = server.URL })
		return providers.NewGitHubProvider(token, opts...)
	}
	t.Cleanup(func() { newGitHubProvider = previousProvider })
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubBranchDelete)), "test-branch-token")
	resultFile := filepath.Join(t.TempDir(), "result.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "reconcile-branches", "--min-age=1h", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if deleteCalls != 0 {
		t.Fatalf("delete calls = %d, want none", deleteCalls)
	}
	if _, err := os.Stat(resultFile); err != nil {
		t.Fatalf("result file: %v", err)
	}
	events := branchReconcileEvents(t, layout.SchedulerDir())
	if len(events) != 1 || events[0].Runner["outcome"] != "candidate" || events[0].Runner["reason"] != "dry-run" {
		t.Fatalf("events = %+v", events)
	}
}

func TestReconcileRemoteBranchesDeletesOnlySafeOwnedCandidate(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runsDir := t.TempDir()
	old := now.Add(-8 * 24 * time.Hour)
	eligible := createBranchReconcileRun(t, runsDir, "implementation", "eligible-run", old, true, true)
	withPR := createBranchReconcileRun(t, runsDir, "implementation", "open-pr-run", old, true, true)
	young := createBranchReconcileRun(t, runsDir, "implementation", "young-run", now.Add(-time.Hour), true, true)
	active := createBranchReconcileRun(t, runsDir, "implementation", "active-run", old, false, true)
	unproven := createBranchReconcileRun(t, runsDir, "implementation", "unproven-run", old, true, false)
	missing := providers.BranchName("implementation", "missing-run")
	malformed := "goobers/implementation/extra/run"

	logDir, log := openBranchReconcileLog(t)
	provider := &fakeBranchReconcileProvider{
		branches: []providers.BranchSummary{
			{Name: eligible, SHA: "eligible-sha"}, {Name: withPR}, {Name: young}, {Name: active},
			{Name: unproven}, {Name: missing}, {Name: malformed},
		},
		openPRs: map[string]providers.PullRequestResult{
			withPR: {ID: "42", Number: 42, URL: "https://github.example/pr/42"},
		},
		deleteResults: map[string]providers.DeleteBranchResult{
			eligible: {Deleted: true},
		},
	}

	report, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    runsDir,
		Prefix:     branchReconcilePrefix,
		Limit:      10,
		MinimumAge: 7 * 24 * time.Hour,
		Delete:     true,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("reconcileRemoteBranches: %v", err)
	}
	if report.Scanned != 7 || report.Candidates != 1 || report.Deleted != 1 ||
		report.Preserved != 6 || report.Ambiguous != 3 || report.Failures != 0 {
		t.Fatalf("report = %+v", report)
	}
	if len(provider.lookupBranches) != 2 {
		t.Fatalf("PR lookups = %v, want only old terminal owned branches", provider.lookupBranches)
	}
	if len(provider.deleteBranches) != 1 || provider.deleteBranches[0] != eligible {
		t.Fatalf("delete calls = %v, want [%s]", provider.deleteBranches, eligible)
	}
	if len(provider.getBranches) != 1 || provider.getBranches[0] != eligible {
		t.Fatalf("tip re-reads = %v, want [%s]", provider.getBranches, eligible)
	}

	events := branchReconcileEvents(t, logDir)
	if len(events) != 8 {
		t.Fatalf("events = %d, want 7 decisions + 1 mutation: %+v", len(events), events)
	}
	decisions := map[string]string{}
	var mutation journal.Event
	for _, event := range events {
		branch, _ := event.Runner["branch"].(string)
		if event.Runner["event"] == "decision" {
			reason, _ := event.Runner["reason"].(string)
			decisions[branch] = reason
		} else if event.Runner["event"] == "mutation" {
			mutation = event
		}
	}
	if decisions[eligible] != "" || decisions[withPR] != "open-pull-request" ||
		decisions[young] != "safety-window" || decisions[active] != "run-active" ||
		decisions[unproven] != "ambiguous-ownership" || decisions[missing] != "ambiguous-ownership" ||
		decisions[malformed] != "ambiguous-ownership" {
		t.Fatalf("decisions = %+v", decisions)
	}
	if mutation.Runner["branch"] != eligible || mutation.Runner["outcome"] != "deleted" {
		t.Fatalf("mutation = %+v", mutation)
	}
}

func TestReconcileRemoteBranchesPreservesConcurrentlyUpdatedTip(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runsDir := t.TempDir()
	branch := createBranchReconcileRun(t, runsDir, "implementation", "updated-tip", now.Add(-8*24*time.Hour), true, true)
	logDir, log := openBranchReconcileLog(t)
	provider := &fakeBranchReconcileProvider{
		branches: []providers.BranchSummary{{Name: branch, SHA: "listed-sha"}},
		current: map[string]providers.BranchSummary{
			branch: {Name: branch, SHA: "pushed-sha"},
		},
	}

	report, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    runsDir,
		Prefix:     branchReconcilePrefix,
		Limit:      25,
		MinimumAge: 7 * 24 * time.Hour,
		Delete:     true,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("reconcileRemoteBranches: %v", err)
	}
	if report.Candidates != 1 || report.Preserved != 1 || report.Deleted != 0 || report.Failures != 0 {
		t.Fatalf("report = %+v", report)
	}
	if len(provider.getBranches) != 1 || provider.getBranches[0] != branch || len(provider.deleteBranches) != 0 {
		t.Fatalf("get calls = %v, delete calls = %v", provider.getBranches, provider.deleteBranches)
	}
	events := branchReconcileEvents(t, logDir)
	if len(events) != 1 || events[0].Runner["outcome"] != "preserved" ||
		events[0].Runner["reason"] != "branch-tip-changed" ||
		events[0].Runner["sha"] != "listed-sha" ||
		events[0].Runner["observedSHA"] != "pushed-sha" {
		t.Fatalf("events = %+v", events)
	}
}

func TestReconcileRemoteBranchesJournalsTipLookupFailure(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runsDir := t.TempDir()
	branch := createBranchReconcileRun(t, runsDir, "implementation", "tip-lookup-failure", now.Add(-8*24*time.Hour), true, true)
	logDir, log := openBranchReconcileLog(t)
	lookupErr := errors.New("ref lookup unavailable")
	provider := &fakeBranchReconcileProvider{
		branches: []providers.BranchSummary{{Name: branch, SHA: "listed-sha"}},
		getErrs:  map[string]error{branch: lookupErr},
	}

	report, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    runsDir,
		Prefix:     branchReconcilePrefix,
		Limit:      25,
		MinimumAge: 7 * 24 * time.Hour,
		Delete:     true,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("reconcileRemoteBranches: %v", err)
	}
	if report.Candidates != 1 || report.Preserved != 1 || report.Deleted != 0 || report.Failures != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(provider.deleteBranches) != 0 {
		t.Fatalf("delete calls = %v, want none", provider.deleteBranches)
	}
	events := branchReconcileEvents(t, logDir)
	if len(events) != 1 || events[0].Runner["outcome"] != "preserved" ||
		events[0].Runner["reason"] != "provider-lookup-failed" ||
		events[0].Error == nil || events[0].Error.Code != "branch_provider_lookup_failed" {
		t.Fatalf("events = %+v", events)
	}
}

func TestReconcileRemoteBranchesJournalsLookupFailuresAndStopsOnRateLimit(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runsDir := t.TempDir()
	first := createBranchReconcileRun(t, runsDir, "implementation", "lookup-failure", now.Add(-8*24*time.Hour), true, true)
	second := createBranchReconcileRun(t, runsDir, "implementation", "rate-limited", now.Add(-8*24*time.Hour), true, true)
	third := createBranchReconcileRun(t, runsDir, "implementation", "not-reached", now.Add(-8*24*time.Hour), true, true)
	logDir, log := openBranchReconcileLog(t)
	rateLimit := &providers.RateLimitError{Endpoint: "pulls", Status: 403, Remaining: 0}
	provider := &fakeBranchReconcileProvider{
		branches: []providers.BranchSummary{{Name: first}, {Name: second}, {Name: third}},
		lookupErrs: map[string]error{
			first:  errors.New("provider unavailable"),
			second: rateLimit,
		},
	}

	report, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    runsDir,
		Prefix:     branchReconcilePrefix,
		Limit:      10,
		MinimumAge: 7 * 24 * time.Hour,
		Delete:     true,
		Now:        func() time.Time { return now },
	})
	if !errors.Is(err, rateLimit) {
		t.Fatalf("error = %v, want rate limit", err)
	}
	if report.Scanned != 2 || report.Preserved != 2 || report.Failures != 2 || report.NextAfter != second {
		t.Fatalf("report = %+v", report)
	}
	if len(provider.deleteBranches) != 0 {
		t.Fatalf("delete calls = %v, want none", provider.deleteBranches)
	}
	events := branchReconcileEvents(t, logDir)
	if len(events) != 2 || events[0].Runner["reason"] != "provider-lookup-failed" ||
		events[1].Runner["reason"] != "rate-limited" ||
		events[1].Error == nil || events[1].Error.Code != providers.ErrorCodeRateLimited {
		t.Fatalf("events = %+v", events)
	}
}

func TestReconcileRemoteBranchesJournalsMutationFailureAndContinues(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runsDir := t.TempDir()
	failed := createBranchReconcileRun(t, runsDir, "implementation", "delete-failure", now.Add(-8*24*time.Hour), true, true)
	deleted := createBranchReconcileRun(t, runsDir, "implementation", "delete-success", now.Add(-8*24*time.Hour), true, true)
	logDir, log := openBranchReconcileLog(t)
	provider := &fakeBranchReconcileProvider{
		branches: []providers.BranchSummary{
			{Name: failed, SHA: "failed-sha"},
			{Name: deleted, SHA: "deleted-sha"},
		},
		deleteErrs: map[string]error{failed: errors.New("delete denied")},
		deleteResults: map[string]providers.DeleteBranchResult{
			deleted: {Deleted: true},
		},
	}

	report, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    runsDir,
		Prefix:     branchReconcilePrefix,
		Limit:      10,
		MinimumAge: 7 * 24 * time.Hour,
		Delete:     true,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("reconcileRemoteBranches: %v", err)
	}
	if report.Candidates != 2 || report.Deleted != 1 || report.Preserved != 1 || report.Failures != 1 {
		t.Fatalf("report = %+v", report)
	}
	events := branchReconcileEvents(t, logDir)
	if len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}
	if events[1].Runner["event"] != "mutation" || events[1].Runner["outcome"] != "failed" ||
		events[1].Error == nil || events[1].Error.Code != "branch_delete_failed" {
		t.Fatalf("failed mutation event = %+v", events[1])
	}
	if events[3].Runner["outcome"] != "deleted" {
		t.Fatalf("successful mutation event = %+v", events[3])
	}
}

func TestReconcileRemoteBranchesPreservesBranchOnDeleteRateLimit(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runsDir := t.TempDir()
	branch := createBranchReconcileRun(t, runsDir, "implementation", "delete-rate-limit", now.Add(-8*24*time.Hour), true, true)
	logDir, log := openBranchReconcileLog(t)
	rateLimit := &providers.RateLimitError{Endpoint: "delete-ref", Status: 429}
	provider := &fakeBranchReconcileProvider{
		branches:   []providers.BranchSummary{{Name: branch, SHA: "branch-sha"}},
		deleteErrs: map[string]error{branch: rateLimit},
	}

	report, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    runsDir,
		Prefix:     branchReconcilePrefix,
		Limit:      10,
		MinimumAge: 7 * 24 * time.Hour,
		Delete:     true,
		Now:        func() time.Time { return now },
	})
	if !errors.Is(err, rateLimit) {
		t.Fatalf("error = %v, want rate limit", err)
	}
	if report.Candidates != 1 || report.Deleted != 0 || report.Preserved != 1 || report.Failures != 1 {
		t.Fatalf("report = %+v", report)
	}
	events := branchReconcileEvents(t, logDir)
	if len(events) != 2 || events[0].Runner["outcome"] != "delete-approved" ||
		events[1].Runner["event"] != "mutation" || events[1].Runner["reason"] != "rate-limited" ||
		events[1].Error == nil || events[1].Error.Code != providers.ErrorCodeRateLimited {
		t.Fatalf("events = %+v", events)
	}
}

func TestReconcileRemoteBranchesEnforcesBatchBound(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runsDir := t.TempDir()
	branches := []providers.BranchSummary{
		{Name: createBranchReconcileRun(t, runsDir, "implementation", "batch-a", now, false, true)},
		{Name: createBranchReconcileRun(t, runsDir, "implementation", "batch-b", now, false, true)},
		{Name: createBranchReconcileRun(t, runsDir, "implementation", "batch-c", now, false, true)},
	}
	_, log := openBranchReconcileLog(t)
	provider := &fakeBranchReconcileProvider{branches: branches}

	report, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    runsDir,
		Prefix:     branchReconcilePrefix,
		After:      "goobers/implementation/prior",
		Limit:      2,
		MinimumAge: 7 * 24 * time.Hour,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("reconcileRemoteBranches: %v", err)
	}
	if report.Scanned != 2 || report.NextAfter != branches[1].Name {
		t.Fatalf("report = %+v", report)
	}
	if len(provider.listRequests) != 1 || provider.listRequests[0].Limit != 2 ||
		provider.listRequests[0].After != "goobers/implementation/prior" {
		t.Fatalf("list requests = %+v", provider.listRequests)
	}
}

func TestReconcileRemoteBranchesJournalsSweepRateLimit(t *testing.T) {
	logDir, log := openBranchReconcileLog(t)
	rateLimit := &providers.RateLimitError{Endpoint: "matching-refs", Status: 429}
	provider := &fakeBranchReconcileProvider{listErr: rateLimit}

	_, err := reconcileRemoteBranches(context.Background(), provider, log, branchReconcileOptions{
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunsDir:    t.TempDir(),
		Prefix:     branchReconcilePrefix,
		Limit:      25,
		MinimumAge: 7 * 24 * time.Hour,
	})
	if !errors.Is(err, rateLimit) {
		t.Fatalf("error = %v, want rate limit", err)
	}
	events := branchReconcileEvents(t, logDir)
	if len(events) != 1 || events[0].Runner["event"] != "sweep" ||
		events[0].Runner["reason"] != "rate-limited" ||
		events[0].Error == nil || events[0].Error.Code != providers.ErrorCodeRateLimited {
		t.Fatalf("events = %+v", events)
	}
}

func TestInspectBranchOwnerRejectsMismatchedRunIdentity(t *testing.T) {
	runsDir := t.TempDir()
	branch := createBranchReconcileRun(t, runsDir, "other-workflow", "identity-run", time.Now(), true, true)
	requested := providers.BranchName("implementation", "identity-run")
	if _, reason, err := inspectBranchOwner(runsDir, branchReconcilePrefix, requested); err == nil || reason != "ambiguous-ownership" {
		t.Fatalf("reason = %q, err = %v", reason, err)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "identity-run"))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := rd.Identity(); err != nil || id.Workflow != "other-workflow" || branch == requested {
		t.Fatalf("identity = %+v, err = %v", id, err)
	}
}
