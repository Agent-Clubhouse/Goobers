package main

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// TestEscalationStillBlocksReadsMergeReviewVerdictPins covers #4031: merge-review
// applies goobers:merge-escalated itself, on a verdict of fail, and that path
// never writes a remediation-state comment — its record rides the verdict-json
// payload on the merge-review-status comment. Before this fix,
// escalationStillBlocks found no remediation-state payload and fell straight
// into its fail-closed branch, so the exit the label's own comment advertises
// ("parked until this PR's head changes") could never fire and every
// merge-review escalation was permanent-until-human.
func TestEscalationStillBlocksReadsMergeReviewVerdictPins(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	failVerdict := func(head, base string) string {
		return renderVerdictComment(apiv1.Verdict{
			Decision: apiv1.VerdictFail,
			Summary:  "the approach itself was rejected",
			HeadSHA:  head,
			BaseSHA:  base,
		})
	}

	t.Run("unchanged head stays blocked", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(1, "pr 1")
		server.addComment(1, failVerdict("head-1", "base-1"))
		server.setBranchTip("main", "base-advanced-well-past-the-snapshot")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 1, HeadSHA: "head-1", BaseSHA: "base-1", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — a fail verdict is a rejection of the PR's content, so no base advance cures it")
		}
	})

	t.Run("head moved unparks", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(2, "pr 2")
		server.addComment(2, failVerdict("head-before", "base-2"))
		server.setBranchTip("main", "base-2")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 2, HeadSHA: "head-after-a-new-commit", BaseSHA: "base-2", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — pushing a commit is the exit the label's own comment advertises")
		}
	})

	t.Run("a remediation-state comment still wins", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(3, "pr 3")
		server.addComment(3, failVerdict("stale-verdict-head", "base-3"))
		comment, err := remediationStateComment(remediationState{
			Escalated: true, EscalatedHeadSHA: "head-3", EscalatedBaseSHA: "base-3",
			EscalationGeneration: 1, EscalationOutcome: remediationOutcomeDidNotConverge,
		})
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		server.addComment(3, comment)
		server.setBranchTip("main", "base-3")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 3, HeadSHA: "head-3", BaseSHA: "base-3", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — pr-remediation's own record is authoritative when present")
		}
	})

	t.Run("a non-escalating verdict is not a snapshot", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(4, "pr 4")
		server.addComment(4, renderVerdictComment(apiv1.Verdict{
			Decision: apiv1.VerdictNeedsChanges,
			Summary:  "routed to remediation, not escalated",
			HeadSHA:  "head-4", BaseSHA: "base-4",
		}))
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 4, HeadSHA: "head-4", BaseSHA: "base-4", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = true expected — needs-changes never escalates, so it carries no park snapshot and the label must still fail closed")
		}
	})

	t.Run("the last fail verdict wins", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(5, "pr 5")
		server.addComment(5, failVerdict("head-old", "base-5"))
		server.addComment(5, failVerdict("head-current", "base-5"))
		server.setBranchTip("main", "base-5")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 5, HeadSHA: "head-current", BaseSHA: "base-5", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — the most recent fail verdict pins the live head, so the PR is unchanged")
		}
	})
}
