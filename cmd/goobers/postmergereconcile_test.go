package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func postMergeReconcileEnv(t *testing.T, serverURL string) string {
	t.Helper()
	root, _ := postMergeEnv(t, serverURL, false, nil)
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubBranchDelete)), "test-token")
	return root
}

func postMergeTestRepo() providers.RepositoryRef {
	return providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
}

func loadPostMergeReconcileEntry(t *testing.T, root string, repo providers.RepositoryRef, pullNumber string) postMergeReconcileEntry {
	t.Helper()
	ledger, err := readPostMergeReconcileLedger(filepath.Join(layoutFor(root).SchedulerDir(), postMergeReconcileLedgerFile))
	if err != nil {
		t.Fatalf("read post-merge reconcile ledger: %v", err)
	}
	return ledger.Entries[postMergeReconcileKey(repo, pullNumber)]
}

func TestReconcilePostMergeProcessesLateMerge(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Fixes #42", []string{"shared/pkg.go"}, []int{21})
	st.setConflicted(21)
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root := postMergeReconcileEnv(t, server.URL)
	repo := postMergeTestRepo()
	if err := recordPostMergeTimeout(root, repo, "20", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("record queue timeout: %v", err)
	}

	code, stdout, stderr := runArgs(t, "reconcile-post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "reconciled 1") {
		t.Fatalf("stdout = %q, want one reconciled pull request", stdout)
	}
	assertLabeledExactly(t, st.labeledSnapshot(), 21)
	st.mu.Lock()
	issueState := st.issueState[42]
	deleteCalls := st.deleteCalls
	st.mu.Unlock()
	if issueState != "closed" {
		t.Fatalf("issue #42 state = %q, want closed", issueState)
	}
	if deleteCalls != 1 {
		t.Fatalf("branch delete calls = %d, want 1", deleteCalls)
	}
	entry := loadPostMergeReconcileEntry(t, root, repo, "20")
	if entry.State != postMergeReconcileCompleted || entry.CompletedAt == nil {
		t.Fatalf("reconcile entry = %+v, want completed", entry)
	}
}

func TestReconcilePostMergeLeavesUnmergedPullRequestPending(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, []int{21})
	st.merged = false
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root := postMergeReconcileEnv(t, server.URL)
	repo := postMergeTestRepo()
	if err := recordPostMergeTimeout(root, repo, "20", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("record queue timeout: %v", err)
	}

	code, stdout, stderr := runArgs(t, "reconcile-post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "still pending 1") {
		t.Fatalf("stdout = %q, want one pending pull request", stdout)
	}
	st.mu.Lock()
	issueState := st.issueState[42]
	deleteCalls := st.deleteCalls
	st.mu.Unlock()
	if issueState == "closed" || deleteCalls != 0 || len(st.labeledSnapshot()) != 0 {
		t.Fatalf("unmerged PR ran post-merge work: issue=%q deleteCalls=%d labels=%v", issueState, deleteCalls, st.labeledSnapshot())
	}
	entry := loadPostMergeReconcileEntry(t, root, repo, "20")
	if entry.State != postMergeReconcilePending || entry.LastCheckedAt == nil {
		t.Fatalf("reconcile entry = %+v, want checked and pending", entry)
	}
}

func TestReconcilePostMergeSkipsAlreadyCompletedPullRequest(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, nil)
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root := postMergeReconcileEnv(t, server.URL)
	repo := postMergeTestRepo()
	if err := recordPostMergeTimeout(root, repo, "20", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("record queue timeout: %v", err)
	}
	ledgerPath := filepath.Join(layoutFor(root).SchedulerDir(), postMergeReconcileLedgerFile)
	ledger, err := readPostMergeReconcileLedger(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if !completePostMergeReconciliation(&ledger, repo, "20") {
		t.Fatal("complete pending reconciliation = false, want true")
	}
	if err := writePostMergeReconcileLedger(ledgerPath, ledger); err != nil {
		t.Fatalf("write completed ledger: %v", err)
	}

	code, stdout, stderr := runArgs(t, "reconcile-post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	st.mu.Lock()
	pollRequests := st.pollRequests
	deleteCalls := st.deleteCalls
	st.mu.Unlock()
	if pollRequests != 0 || deleteCalls != 0 || len(st.labeledSnapshot()) != 0 {
		t.Fatalf("completed PR repeated work: polls=%d deletes=%d labels=%v", pollRequests, deleteCalls, st.labeledSnapshot())
	}
}

func TestReconcilePostMergeContinuesBookkeepingWhenBranchCleanupFails(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, nil)
	st.deleteStatus = http.StatusUnprocessableEntity
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root := postMergeReconcileEnv(t, server.URL)
	repo := postMergeTestRepo()
	if err := recordPostMergeTimeout(root, repo, "20", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("record queue timeout: %v", err)
	}

	code, _, stderr := runArgs(t, "reconcile-post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "branch cleanup failed") {
		t.Fatalf("stderr = %q, want branch cleanup warning", stderr)
	}
	st.mu.Lock()
	issueState := st.issueState[42]
	st.mu.Unlock()
	if issueState != "closed" {
		t.Fatalf("issue #42 state = %q, want close-out despite branch cleanup failure", issueState)
	}
	if entry := loadPostMergeReconcileEntry(t, root, repo, "20"); entry.State != postMergeReconcileCompleted {
		t.Fatalf("reconcile entry = %+v, want completed", entry)
	}
}

func TestReconcilePostMergeRejectsUnboundedBatch(t *testing.T) {
	code, _, stderr := runArgs(t, "reconcile-post-merge", "--max", "101")
	if code != 1 || !strings.Contains(stderr, "max must be between 1 and 100") {
		t.Fatalf("code = %d, stderr = %q, want bounded-batch rejection", code, stderr)
	}
}
