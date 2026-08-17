package main

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestElectionIneligibleSetReevaluatesSnapshotEligibility(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(238, "active escalation")
	server.addIssue(239, "eligible")
	server.addIssue(241, "stale escalation")

	active, err := remediationStateComment(remediationState{
		Escalated:            true,
		EscalatedHeadSHA:     "head-238",
		EscalatedBaseSHA:     "base",
		EscalatedReason:      "human intervention required",
		EscalationGeneration: 1,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	stale, err := remediationStateComment(remediationState{
		Escalated:            true,
		EscalatedHeadSHA:     "old-head-241",
		EscalatedBaseSHA:     "base",
		EscalationGeneration: 1,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server.addComment(238, active)
	server.addComment(241, stale)

	prs := []providers.PullRequestSummary{
		{Number: 238, HeadSHA: "head-238", Labels: []string{remediationEscalatedLabel}},
		{Number: 239, HeadSHA: "head-239"},
		{Number: 240, HeadSHA: "head-240", Labels: []string{needsHumanLabel}},
		{Number: 241, HeadSHA: "new-head-241", Labels: []string{remediationEscalatedLabel}},
	}
	got, err := electionIneligibleSet(context.Background(), server.newGitHubProvider("token"), repo, prs)
	if err != nil {
		t.Fatalf("electionIneligibleSet: %v", err)
	}
	if !got[238] || !got[240] {
		t.Fatalf("ineligible = %v, want active escalation #238 and needs-human #240", got)
	}
	if got[239] || got[241] {
		t.Fatalf("ineligible = %v, want eligible #239 and snapshot-healed #241", got)
	}

	prs[0].HeadSHA = "new-head-238"
	prs[2].Labels = nil
	got, err = electionIneligibleSet(context.Background(), server.newGitHubProvider("token"), repo, prs)
	if err != nil {
		t.Fatalf("electionIneligibleSet after eligibility change: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ineligible after eligibility change = %v, want empty", got)
	}
}

func TestElectionCrownsLowestCurrentlyEligiblePR(t *testing.T) {
	findings := []apiv1.Finding{{
		Class:       apiv1.FindingCrossPRBlocked,
		BlockingPRs: []int{238, 241},
	}}

	if electionDecision(findings, 239, electedLander, nil) {
		t.Fatal("PR #239 must wait while lower PR #238 is eligible")
	}
	if !electionDecision(findings, 239, electedLander, map[int]bool{238: true}) {
		t.Fatal("PR #239 must be crowned when lower PR #238 becomes ineligible")
	}
	if got := predecessorBlockers(239, []int{238, 241}, electedLander, map[int]bool{238: true}); len(got) != 0 {
		t.Fatalf("predecessorBlockers = %v, want none after #238 becomes ineligible", got)
	}
}
