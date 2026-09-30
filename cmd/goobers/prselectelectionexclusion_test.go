package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// The FIFO-cluster deadlock observed live on v0.5.0-beta.5/beta.6: the oldest
// cluster member (#73) is parked outside the landing loop, so the election
// drops it and crowns the next member (#74). But #74 carries
// goobers:blocked-on-sibling with #73 as its recorded blocker, and pr-select
// only discounted *unlandable* blockers (#5602), so it held #74 behind #73
// forever. Every newer overlapping PR then deferred to #74 and nothing landed.
// pr-select must discount exactly the blockers the election discounts.
const (
	exclusionBlocker = 73
	exclusionHeld    = 74
)

func runPRSelectForExclusion(t *testing.T, root string, server *fakeGitHubServer) (int, string, string) {
	t.Helper()
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-election-exclusion")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), filepath.Join(workDir, "selected-pr.json"))
	return runArgs(t, "pr-select", root)
}

func addHeldBehindBlocker(t *testing.T, server *fakeGitHubServer, blockerLabels []string) {
	t.Helper()
	server.addIssue(exclusionBlocker, "oldest cluster member")
	server.addOpenPR(exclusionBlocker, "goobers/implementation/run-73", "main", "sha73", "base", false, blockerLabels, nil)
	server.addIssue(exclusionHeld, "held cluster member")
	server.addOpenPR(exclusionHeld, "goobers/implementation/run-74", "main", "sha74", "base", false,
		[]string{blockedOnSiblingLabel}, nil)
	server.addComment(exclusionHeld, blockedOnSiblingCommentFor(t, exclusionBlocker))
}

func TestPRSelectSelectsPRHeldBehindNeedsHumanBlocker(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	addHeldBehindBlocker(t, server, []string{providers.LabelNeedsHuman})

	code, stdout, stderr := runPRSelectForExclusion(t, root, server)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "selected PR #74") {
		t.Fatalf("stdout = %q, want PR #74 selected: its only recorded blocker is parked needs-human and cannot be elected", stdout)
	}
}

func TestPRSelectSelectsPRHeldBehindSubstantivelyEscalatedBlocker(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	addHeldBehindBlocker(t, server, []string{remediationEscalatedLabel})
	escalation, err := remediationStateComment(remediationState{
		Escalated:            true,
		EscalatedHeadSHA:     "sha73",
		EscalatedBaseSHA:     "base",
		EscalationCauses:     []remediationCause{remediationCauseSubstantive},
		EscalationGeneration: 1,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server.addComment(exclusionBlocker, escalation)

	code, stdout, stderr := runPRSelectForExclusion(t, root, server)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "selected PR #74") {
		t.Fatalf("stdout = %q, want PR #74 selected: its only recorded blocker's substantive escalation still blocks at an unchanged head, so the election drops it", stdout)
	}
}

// Control: a recorded blocker the election still considers a candidate keeps
// holding its dependent, so the fix only discounts blockers that cannot land.
func TestPRSelectStillHoldsPRBehindLandableBlocker(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	addHeldBehindBlocker(t, server, nil)

	code, stdout, stderr := runPRSelectForExclusion(t, root, server)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "selected PR #74") {
		t.Fatalf("stdout = %q, PR #74 must stay held behind its live, landable blocker #73", stdout)
	}
}
