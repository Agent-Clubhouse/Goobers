package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

const (
	replayGitHubBranchDelete = "DELETE /repos/your-org/your-repo/git/refs/heads/goobers/implementation/run-merged"
)

// replayReconcilePostMergeGitHub replays reconcile-post-merge on a late merge
// of PR #20 ("Fixes #42") that carries one earlier run's cost receipt.
//
// A reconciliation that finished is a no-op on its next run, so the retry that
// matters is the one after a crash inside the batch: run 2 starts from the
// ledger as it stood after the branch, fan-out and unpark checkpoints but
// before issue #42's close-out checkpoint, so every provider write run 1
// made is already on the provider under its attribution footer. Run 2
// recollects the cost report, so it must find its attributed PR cost summary
// by marker, and it must find its attributed "Merged in pull request #20."
// close-out by its first line, and post neither again.
func replayReconcilePostMergeGitHub(t *testing.T) replayFixture {
	st := newPostMergeServerState(20, "main", "Fixes #42", nil, nil)
	st.prComments = []string{costComment(t, "goobers", "implementation", "cost-run", 20, 8_000_000_000).Body}
	fake := newPostMergeServer(t, "your-org", "your-repo", st)
	counter := &providerWriteCounter{}
	server := recordServerWrites(t, fake.Config.Handler, counter)
	root := postMergeReconcileEnv(t, server.URL)
	t.Setenv(executor.GaggleEnvVar, "")
	setTestInstanceCostPublication(t, root, true)
	repo := postMergeTestRepo()
	if err := recordPostMergeTimeout(root, repo, "20", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("record queue timeout: %v", err)
	}
	ledgerPath := filepath.Join(layoutFor(root).SchedulerDir(), postMergeReconcileLedgerFile)
	key := postMergeReconcileKey(repo, "20")
	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			t.Helper()
			rewindPostMergeReconcileCloseOut(t, ledgerPath, key)
			return runArgs(t, "reconcile-post-merge", root)
		},
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			st.mu.Lock()
			defer st.mu.Unlock()
			owned := map[string][]string{"cost summary": nil, "issue close-out": nil}
			for _, comment := range st.prComments {
				if strings.Contains(providers.StripAttribution(comment), postMergeCostSummaryMarker) {
					owned["cost summary"] = append(owned["cost summary"], comment)
				}
			}
			for _, comment := range st.issueComments[42] {
				if strings.HasPrefix(providers.StripAttribution(comment), "Merged in pull request #20.") {
					owned["issue close-out"] = append(owned["issue close-out"], comment)
				}
			}
			if st.issueState[42] != "closed" {
				t.Errorf("issue #42 state = %q, want closed", st.issueState[42])
			}
			return owned
		},
	}
}

// rewindPostMergeReconcileCloseOut puts a completed reconciliation back to
// where a crash after its close-out comment but before the close-out
// checkpoint leaves it: pending, every earlier action checkpointed, no issue
// recorded closed. A pending entry is left alone, so run 1 starts fresh.
func rewindPostMergeReconcileCloseOut(t *testing.T, ledgerPath, key string) {
	t.Helper()
	ledger, err := readPostMergeReconcileLedger(ledgerPath)
	if err != nil {
		t.Fatalf("read post-merge reconcile ledger: %v", err)
	}
	entry, ok := ledger.Entries[key]
	if !ok {
		t.Fatalf("post-merge reconcile ledger has no entry %q", key)
	}
	if entry.State != postMergeReconcileCompleted {
		return
	}
	entry.State = postMergeReconcilePending
	entry.CompletedAt = nil
	entry.Actions.ClosedIssueNumbers = nil
	ledger.Entries[key] = entry
	if err := writePostMergeReconcileLedger(ledgerPath, ledger); err != nil {
		t.Fatalf("rewind post-merge reconcile ledger: %v", err)
	}
}
