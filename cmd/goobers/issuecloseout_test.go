package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// TestIssueCloseOutCommentsClosesAndReleasesClaim is #132's issue-close-out
// CLI-level acceptance: invoking `goobers issue-close-out` via the actual
// CLI entrypoint recovers which item its own run claimed (from the claim
// ledger — issue-close-out has no other way to learn it, several stages and
// worktrees after backlog-query), finds the run's PR by its stable branch
// name, comments + marks the issue done, and releases the claim early
// instead of waiting for its lease to expire.
func TestIssueCloseOutCommentsClosesAndReleasesClaim(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	const runID = "run-1"
	const workflow = "implementation"

	// Seed the claim ledger as if backlog-query already claimed item 7 for
	// this run (its own worktree — and claimed-item.json in it — is long
	// gone by the time issue-close-out runs).
	schedulerDir := filepath.Join(root, "scheduler")
	if err := (func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
		if err != nil {
			return err
		}
		_, _, err = ledger.Claim("7", runID, workflow, time.Hour)
		return err
	})(); err != nil {
		t.Fatalf("seed claim ledger: %v", err)
	}

	// Seed an open PR on the run's deterministic branch, as open-pr would
	// have created it.
	head := providers.BranchName(workflow, runID)
	server.mu.Lock()
	server.prs[1] = &fakePR{number: 1, title: "Implementation", head: head, base: "main", state: "open"}
	server.nextPR = 2
	server.mu.Unlock()

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", runID)
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "issue-close-out", root)
	if code != 0 {
		t.Fatalf("issue-close-out: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "closed out 7") {
		t.Fatalf("stdout = %q, want a mention of the closed-out item", stdout)
	}

	server.mu.Lock()
	issue := server.issues[7]
	server.mu.Unlock()
	if issue.state != "closed" {
		t.Fatalf("issue state = %q, want closed", issue.state)
	}
	if len(issue.comments) != 1 || !strings.Contains(issue.comments[0], "https://example/pull/1") {
		t.Fatalf("issue comments = %+v, want exactly one linking pull/1", issue.comments)
	}

	// The claim was released, not left to expire.
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, ok := ledger.ForRun(runID); ok {
		t.Fatal("expected the claim to be released after close-out")
	}
}
