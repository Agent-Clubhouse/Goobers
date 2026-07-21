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

func TestReconcilePostMergeRetriesFailedBranchCleanupWithoutRepeatingCompletedActions(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, nil)
	st.deleteStatus = http.StatusUnprocessableEntity
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root := postMergeReconcileEnv(t, server.URL)
	repo := postMergeTestRepo()
	if err := recordPostMergeTimeout(root, repo, "20", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("record queue timeout: %v", err)
	}

	code, _, stderr := runArgs(t, "reconcile-post-merge", root)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want retryable failure", code, stderr)
	}
	if !strings.Contains(stderr, "branch cleanup:") {
		t.Fatalf("stderr = %q, want branch cleanup failure", stderr)
	}
	st.mu.Lock()
	issueState := st.issueState[42]
	issueComments := append([]string(nil), st.issueComments[42]...)
	st.mu.Unlock()
	if issueState != "closed" {
		t.Fatalf("issue #42 state = %q, want close-out despite branch cleanup failure", issueState)
	}
	if len(issueComments) != 1 {
		t.Fatalf("issue #42 comments = %v, want one completed close-out", issueComments)
	}
	entry := loadPostMergeReconcileEntry(t, root, repo, "20")
	if entry.State != postMergeReconcilePending || entry.Actions.BranchCleanup || !entry.Actions.ClosedIssueNumbers["42"] {
		t.Fatalf("reconcile entry = %+v, want only branch cleanup pending", entry)
	}

	st.mu.Lock()
	st.deleteStatus = 0
	st.mu.Unlock()
	code, _, stderr = runArgs(t, "reconcile-post-merge", root)
	if code != 0 {
		t.Fatalf("retry code = %d, stderr = %q", code, stderr)
	}
	st.mu.Lock()
	issueComments = append([]string(nil), st.issueComments[42]...)
	deleteCalls := st.deleteCalls
	st.mu.Unlock()
	if len(issueComments) != 1 {
		t.Fatalf("issue #42 comments after retry = %v, want no duplicate", issueComments)
	}
	if deleteCalls != 2 {
		t.Fatalf("branch delete calls = %d, want failed attempt plus successful retry", deleteCalls)
	}
	if entry = loadPostMergeReconcileEntry(t, root, repo, "20"); entry.State != postMergeReconcileCompleted {
		t.Fatalf("reconcile entry after retry = %+v, want completed", entry)
	}
}

func TestReconcilePostMergeDeduplicatesCloseOutAfterInterruptedCheckpoint(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, nil)
	st.issueState[42] = "closed"
	st.issueLabels[42] = []string{"goobers/status:done"}
	st.issueComments[42] = []string{"Merged in pull request #20."}
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
	key := postMergeReconcileKey(repo, "20")
	entry := ledger.Entries[key]
	entry.Actions = postMergeReconcileActions{
		BranchCleanup:    true,
		SiblingFanOut:    true,
		ResolvedUnpark:   true,
		EscalationUnpark: true,
		DemotionUnpark:   true,
	}
	ledger.Entries[key] = entry
	if err := writePostMergeReconcileLedger(ledgerPath, ledger); err != nil {
		t.Fatalf("write interrupted ledger: %v", err)
	}

	code, _, stderr := runArgs(t, "reconcile-post-merge", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	st.mu.Lock()
	issueComments := append([]string(nil), st.issueComments[42]...)
	deleteCalls := st.deleteCalls
	st.mu.Unlock()
	if len(issueComments) != 1 {
		t.Fatalf("issue #42 comments = %v, want interrupted close-out deduplicated", issueComments)
	}
	if deleteCalls != 0 || len(st.labeledSnapshot()) != 0 {
		t.Fatalf("checkpointed actions repeated: deleteCalls=%d labels=%v", deleteCalls, st.labeledSnapshot())
	}
	entry = loadPostMergeReconcileEntry(t, root, repo, "20")
	if entry.State != postMergeReconcileCompleted || !entry.Actions.ClosedIssueNumbers["42"] {
		t.Fatalf("reconcile entry = %+v, want close-out checkpointed and completed", entry)
	}
}

func TestReconcilePostMergeRetriesFailedFanOutAndCloseOut(t *testing.T) {
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, []int{21})
	st.setConflicted(21)
	st.labelStatus = http.StatusUnprocessableEntity
	st.commentStatus = http.StatusUnprocessableEntity
	server := newPostMergeServer(t, "your-org", "your-repo", st)
	root := postMergeReconcileEnv(t, server.URL)
	repo := postMergeTestRepo()
	if err := recordPostMergeTimeout(root, repo, "20", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("record queue timeout: %v", err)
	}

	code, _, stderr := runArgs(t, "reconcile-post-merge", root)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want retryable failure", code, stderr)
	}
	if !strings.Contains(stderr, "sibling fan-out:") || !strings.Contains(stderr, "close issue #42:") {
		t.Fatalf("stderr = %q, want fan-out and close-out failures", stderr)
	}
	entry := loadPostMergeReconcileEntry(t, root, repo, "20")
	if entry.State != postMergeReconcilePending ||
		!entry.Actions.BranchCleanup ||
		entry.Actions.SiblingFanOut ||
		entry.Actions.ClosedIssueNumbers["42"] {
		t.Fatalf("reconcile entry = %+v, want fan-out and close-out pending", entry)
	}

	st.mu.Lock()
	st.labelStatus = 0
	st.commentStatus = 0
	st.mu.Unlock()
	code, _, stderr = runArgs(t, "reconcile-post-merge", root)
	if code != 0 {
		t.Fatalf("retry code = %d, stderr = %q", code, stderr)
	}
	assertLabeledExactly(t, st.labeledSnapshot(), 21)
	st.mu.Lock()
	issueComments := append([]string(nil), st.issueComments[42]...)
	deleteCalls := st.deleteCalls
	st.mu.Unlock()
	if len(issueComments) != 1 {
		t.Fatalf("issue #42 comments = %v, want one successful retry", issueComments)
	}
	if deleteCalls != 1 {
		t.Fatalf("branch delete calls = %d, want completed cleanup checkpoint skipped", deleteCalls)
	}
	if entry = loadPostMergeReconcileEntry(t, root, repo, "20"); entry.State != postMergeReconcileCompleted {
		t.Fatalf("reconcile entry after retry = %+v, want completed", entry)
	}
}

func TestReconcilePostMergeRejectsUnboundedBatch(t *testing.T) {
	code, _, stderr := runArgs(t, "reconcile-post-merge", "--max", "101")
	if code != 1 || !strings.Contains(stderr, "max must be between 1 and 100") {
		t.Fatalf("code = %d, stderr = %q, want bounded-batch rejection", code, stderr)
	}
}
