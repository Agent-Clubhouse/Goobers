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

// TestFindinglessPassSiblingUnparksOnBaseAdvance covers #4051, the half of
// #4038 its first fix missed. The cluster election escalates a deferring
// sibling AFTER that sibling's own merge-review verdict, and when that verdict
// is a pass it carries no findings at all. #4038 derived the cause class with
// allCrossPRBlocked, which returns false for an empty finding list by design,
// so the pass case recorded no EscalationCauses, escalationBaseAdvanceUnparks
// stayed false, and the park was released only by a head change that
// pr-remediation — excluded upstream — could never produce.
//
// Live on 2026-08-31, after #4038 had shipped to 6d044284195c, targeted
// `goobers run --pr N pr-remediation` still refused #3941, #3894, #3900,
// #3891 and #3968 at filterRemediationPullRequests. #3894 is the sharpest:
// verdict pass at 19:25Z, goobers:merge-escalated applied at 20:45Z, head
// still exactly what the pass pinned, base advanced out from under it to
// CONFLICTING.
func TestFindinglessPassSiblingUnparksOnBaseAdvance(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	passVerdict := func(head, base string) string {
		return renderVerdictComment(apiv1.Verdict{
			Decision: apiv1.VerdictPass,
			Summary:  "waiter-leak fix is correct on every exit",
			HeadSHA:  head,
			BaseSHA:  base,
		})
	}

	t.Run("base advanced under a finding-less pass unparks it", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(7, "pr 7")
		server.addComment(7, passVerdict("h7", "b7"))
		server.setBranchTip("main", "advanced-base")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 7, HeadSHA: "h7", BaseSHA: "b7", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — a pass faults nothing in this PR, so the escalation is the cluster's ordering and a base advance cures it")
		}
	})

	t.Run("finding-less pass at an unchanged base stays parked", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(8, "pr 8")
		server.addComment(8, passVerdict("h8", "b8"))
		server.setBranchTip("main", "b8")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 8, HeadSHA: "h8", BaseSHA: "b8", Base: "main",
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

	t.Run("a sibling-attributed needs-changes is not this PR's own content", func(t *testing.T) {
		// The reviewer faulted the SIBLING, and pointed Location at it with no
		// mention of this PR. That is not a rejection of this PR's content, so
		// a base advance must release it just as a pass does.
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(9, "pr 9")
		server.addComment(9, renderVerdictComment(apiv1.Verdict{
			Decision: apiv1.VerdictNeedsChanges,
			Summary:  "ordering only",
			HeadSHA:  "h9",
			BaseSHA:  "b9",
			Findings: []apiv1.Finding{{
				Class:    apiv1.FindingCrossPRBlocked,
				Severity: apiv1.SeverityError,
				Message:  "merge PR #99 first, then rebase",
				Location: "PR #99",
			}},
		}))
		server.setBranchTip("main", "advanced-base")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 9, HeadSHA: "h9", BaseSHA: "b9", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — #4038's original ordering case must keep working under the new predicate")
		}
	})

	t.Run("a finding-less needs-changes still fails closed", func(t *testing.T) {
		// The complement of the pass case, and #4031's contract: a REJECTING
		// verdict with no findings carries no attribution either way, so it
		// must not be read as "nothing is wrong about this PR".
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(11, "pr 11")
		server.addComment(11, renderVerdictComment(apiv1.Verdict{
			Decision: apiv1.VerdictNeedsChanges,
			Summary:  "rejected without an itemised finding",
			HeadSHA:  "h11",
			BaseSHA:  "b11",
		}))
		server.setBranchTip("main", "advanced-base")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 11, HeadSHA: "h11", BaseSHA: "b11", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — an empty finding list on a rejecting verdict is not evidence that the PR is fine")
		}
	})

	t.Run("an info-severity self-attributed finding still parks", func(t *testing.T) {
		// The severity floor is deliberately the lowest one: any finding that
		// blames this PR's own diff keeps the park, because un-parking a PR a
		// reviewer really did fault is the worse error.
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(10, "pr 10")
		server.addComment(10, renderVerdictComment(apiv1.Verdict{
			Decision: apiv1.VerdictNeedsChanges,
			Summary:  "small but real",
			HeadSHA:  "h10",
			BaseSHA:  "b10",
			Findings: []apiv1.Finding{{
				Class:    apiv1.FindingMissingTests,
				Severity: apiv1.SeverityInfo,
				Message:  "this branch is untested",
				Location: "cmd/goobers/thing.go:10",
			}},
		}))
		server.setBranchTip("main", "advanced-base")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{
			Number: 10, HeadSHA: "h10", BaseSHA: "b10", Base: "main",
			Labels: []string{remediationEscalatedLabel},
		}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — a self-attributed finding at any severity keeps the park")
		}
	})
}
