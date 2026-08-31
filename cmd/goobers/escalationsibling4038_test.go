package main

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// TestClusterSiblingEscalationHasAnExit covers #4038: a PR escalated by the
// CLUSTER election carries goobers:merge-escalated while the escalating fail
// verdict lives on the cluster WINNER's thread. The sibling's own thread holds
// only its own needs-changes or pass verdict, so latestRemediationState finds
// nothing and latestMergeReviewEscalationPins — which matches fail only —
// finds nothing either. Before this fix that fell through to the fail-closed
// return: the park had no exit at all, because pr-remediation excludes
// escalated PRs upstream, so the head could never move to release it.
//
// Live on 2026-08-31 that was four of the five open bot PRs (#3941, #3900,
// #3894, #3891), three of them carrying a verdict of pass, and it starved
// pr-remediation to `no work: no PR needs remediation this cycle` on every
// tick.
func TestClusterSiblingEscalationHasAnExit(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	siblingVerdict := func(decision apiv1.VerdictDecision, head, base string, findings []apiv1.Finding) string {
		return renderVerdictComment(apiv1.Verdict{
			Decision: decision,
			Summary:  "cluster sibling",
			HeadSHA:  head,
			BaseSHA:  base,
			Findings: findings,
		})
	}
	orderingFinding := []apiv1.Finding{{
		Class:    apiv1.FindingCrossPRBlocked,
		Severity: apiv1.SeverityError,
		Message:  "sibling PR #2 changes the same file; the two must be ordered",
		Location: "PR #2",
	}}
	substantiveFinding := []apiv1.Finding{{
		Class:    apiv1.FindingMissingTests,
		Severity: apiv1.SeverityError,
		Message:  "the new branch has no test coverage",
		Location: "cmd/goobers/thing.go:10",
	}}

	t.Run("head moved since the sibling's own verdict unparks it", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(1, "pr 1")
		server.addComment(1, siblingVerdict(apiv1.VerdictNeedsChanges, "old-head", "b1", substantiveFinding))
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 1, HeadSHA: "new-head", BaseSHA: "b1",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — pushing a commit must release a cluster-sibling park")
		}
	})

	t.Run("unchanged head with a substantive verdict stays parked", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(2, "pr 2")
		server.addComment(2, siblingVerdict(apiv1.VerdictNeedsChanges, "h2", "b2", substantiveFinding))
		server.setBranchTip("main", "advanced-base")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 2, HeadSHA: "h2", BaseSHA: "b2", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — a base advance must not cure a content rejection at an unchanged head")
		}
	})

	t.Run("pure ordering escalation unparks when the base advances", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(3, "pr 3")
		server.addComment(3, siblingVerdict(apiv1.VerdictPass, "h3", "b3", orderingFinding))
		server.setBranchTip("main", "advanced-base")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 3, HeadSHA: "h3", BaseSHA: "b3", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — the sibling this PR deferred to has landed on the base")
		}
	})

	t.Run("pure ordering escalation stays parked while the base is unchanged", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(4, "pr 4")
		server.addComment(4, siblingVerdict(apiv1.VerdictPass, "h4", "b4", orderingFinding))
		server.setBranchTip("main", "b4")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 4, HeadSHA: "h4", BaseSHA: "b4", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — nothing has moved, so the park is still correct")
		}
	})

	t.Run("a fail verdict on the PR's own thread still wins the fallback", func(t *testing.T) {
		// The #4032 path must keep precedence: when the PR was escalated by
		// its OWN fail, that verdict's pins are the snapshot, and the later
		// pass must not resurrect a different one.
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(5, "pr 5")
		server.addComment(5, siblingVerdict(apiv1.VerdictFail, "h5", "b5", substantiveFinding))
		server.addComment(5, siblingVerdict(apiv1.VerdictPass, "later-head", "later-base", orderingFinding))
		server.setBranchTip("main", "advanced-base")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 5, HeadSHA: "h5", BaseSHA: "b5", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — the fail pins still match, so the park holds")
		}
	})

	t.Run("no verdict payload at all still fails closed", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(6, "pr 6")
		server.addComment(6, "please rebase, thanks!")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 6, HeadSHA: "h6", BaseSHA: "b6",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — a hand-applied label with no snapshot must fail closed")
		}
	})
}
