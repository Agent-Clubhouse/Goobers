package main

import (
	"context"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// TestDemotionStillHolds exercises #950's self-heal decision in isolation,
// mirroring TestEscalationStillBlocks: an unlabeled PR is never demoted; a
// labeled PR with no recorded snapshot fails closed (stays demoted until a human
// clears it); a labeled PR whose snapshot head still matches stays demoted; a
// labeled PR whose head has advanced past the snapshot self-heals.
func TestDemotionStillHolds(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	t.Run("no label, never demoted", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(1, "pr 1")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 1, HeadSHA: "h1"}
		held, err := demotionStillHolds(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("demotionStillHolds: %v", err)
		}
		if held {
			t.Fatal("held = true, want false — PR carries no merge-demoted label")
		}
	})

	t.Run("labeled but no snapshot fails closed", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(2, "pr 2")
		server.addComment(2, "please look at this, thanks!")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 2, HeadSHA: "h2", Labels: []string{mergeDemotedLabel}}
		held, err := demotionStillHolds(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("demotionStillHolds: %v", err)
		}
		if !held {
			t.Fatal("held = false, want true — labeled with no snapshot must fail closed")
		}
	})

	t.Run("unchanged head stays demoted", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(3, "pr 3")
		c, err := mergeDemotionComment(mergeDemotionState{Attempts: 3, Demoted: true, HeadSHA: "h3"})
		if err != nil {
			t.Fatalf("mergeDemotionComment: %v", err)
		}
		server.addComment(3, c)
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 3, HeadSHA: "h3", Labels: []string{mergeDemotedLabel}}
		held, err := demotionStillHolds(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("demotionStillHolds: %v", err)
		}
		if !held {
			t.Fatal("held = false, want true — head unchanged since demotion")
		}
	})

	t.Run("advanced head self-heals", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(4, "pr 4")
		c, err := mergeDemotionComment(mergeDemotionState{Attempts: 3, Demoted: true, HeadSHA: "old-head"})
		if err != nil {
			t.Fatalf("mergeDemotionComment: %v", err)
		}
		server.addComment(4, c)
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 4, HeadSHA: "new-head", Labels: []string{mergeDemotedLabel}}
		held, err := demotionStillHolds(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("demotionStillHolds: %v", err)
		}
		if held {
			t.Fatal("held = true, want false — head advanced past the snapshot, must self-heal")
		}
	})
}

func TestWithoutDemoted(t *testing.T) {
	tests := []struct {
		name     string
		blockers []int
		demoted  map[int]bool
		want     []int
	}{
		{"empty demoted is identity", []int{5, 6, 7}, nil, []int{5, 6, 7}},
		{"drops a demoted blocker", []int{5, 6, 7}, map[int]bool{6: true}, []int{5, 7}},
		{"drops several", []int{5, 6, 7}, map[int]bool{5: true, 7: true}, []int{6}},
		{"all demoted -> empty", []int{5, 6}, map[int]bool{5: true, 6: true}, []int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withoutDemoted(tt.blockers, tt.demoted)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("withoutDemoted(%v, %v) = %v, want %v", tt.blockers, tt.demoted, got, tt.want)
			}
		})
	}
}

// TestElectionDecisionDemotesStuckLander is #950's core: when the FIFO-minimum
// lander of a cluster is demoted (it repeatedly could not merge at an unchanged
// head), it is dropped from candidacy AND from its siblings' blocker sets, so
// the next-lowest non-demoted member is crowned and the cluster drains around
// the stuck one instead of deadlocking behind it forever.
func TestElectionDecisionDemotesStuckLander(t *testing.T) {
	crossPR := func(blockers ...int) apiv1.Finding {
		return apiv1.Finding{Class: apiv1.FindingCrossPRBlocked, BlockingPRs: blockers}
	}
	// Cluster {5, 6, 7}; every member overlaps the other two.
	findingsFor6 := []apiv1.Finding{crossPR(5, 7)}
	findingsFor5 := []apiv1.Finding{crossPR(6, 7)}

	// Baseline (no demotions): 5 is the FIFO winner, 6 parks behind it.
	if electionDecision(findingsFor6, 6, electedLander, nil) {
		t.Fatal("baseline: PR #6 should NOT be crowned while #5 is a live blocker")
	}
	if !electionDecision(findingsFor5, 5, electedLander, nil) {
		t.Fatal("baseline: PR #5 (FIFO minimum) should be crowned")
	}

	// #5 demoted: it is never crowned, and #6 now wins (only #7 remains as a
	// blocker, and 6 < 7).
	demoted := map[int]bool{5: true}
	if electionDecision(findingsFor5, 5, electedLander, demoted) {
		t.Fatal("a demoted lander (#5) must never be crowned")
	}
	if !electionDecision(findingsFor6, 6, electedLander, demoted) {
		t.Fatal("with #5 demoted, PR #6 must be crowned so the cluster drains around #5")
	}
}

// TestPredecessorBlockersSkipsDemoted proves the parked side agrees with the
// election: once #5 is demoted, #6 records no predecessor waiting on #5, so it
// unparks and becomes selectable (elect-lander and apply-verdict must never
// disagree, or a PR is crowned by one and parked by the other).
func TestPredecessorBlockersSkipsDemoted(t *testing.T) {
	// Without demotion, #6's predecessor in {5,6,7} is #5.
	if got := predecessorBlockers(6, []int{5, 7}, electedLander, nil); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("baseline predecessorBlockers(6, [5 7]) = %v, want [5]", got)
	}
	// With #5 demoted, #6 has no predecessor (7 is higher), so it unparks.
	if got := predecessorBlockers(6, []int{5, 7}, electedLander, map[int]bool{5: true}); len(got) != 0 {
		t.Fatalf("predecessorBlockers(6, [5 7]) with #5 demoted = %v, want none", got)
	}
}

func TestElectionIneligibleSet(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)

	server.addIssue(5, "active escalation")
	server.addOpenPR(5, "goobers/implementation/5", "main", "h5", "base", false, []string{remediationEscalatedLabel}, nil)
	active, err := remediationStateComment(remediationState{
		Escalated:            true,
		EscalatedHeadSHA:     "h5",
		EscalatedBaseSHA:     "base",
		EscalationGeneration: 1,
		EscalationCauses:     []remediationCause{remediationCauseSubstantive},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.addComment(5, active)

	server.addIssue(6, "self-healed escalation")
	server.addOpenPR(6, "goobers/implementation/6", "main", "new-h6", "base", false, []string{remediationEscalatedLabel}, nil)
	healed, err := remediationStateComment(remediationState{
		Escalated:            true,
		EscalatedHeadSHA:     "old-h6",
		EscalatedBaseSHA:     "base",
		EscalationGeneration: 1,
		EscalationCauses:     []remediationCause{remediationCauseSubstantive},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.addComment(6, healed)

	server.addIssue(7, "needs human")
	server.addOpenPR(7, "goobers/implementation/7", "main", "h7", "base", false, []string{providers.LabelNeedsHuman}, nil)

	server.addIssue(8, "human chooses cluster order")
	server.addOpenPR(8, "goobers/implementation/8", "main", "h8", "base", false, []string{remediationEscalatedLabel}, nil)
	server.addComment(8, renderVerdictComment(apiv1.Verdict{
		Decision:  apiv1.VerdictFail,
		Rationale: noLanderEscalationPrefix + ` "fifo": human intervention is required`,
	}))

	server.addIssue(9, "spoofed human-order escalation")
	server.addOpenPR(9, "goobers/implementation/9", "main", "h9", "base", false, []string{remediationEscalatedLabel}, nil)
	spoofed, err := remediationStateComment(remediationState{
		Escalated:            true,
		EscalatedHeadSHA:     "h9",
		EscalatedBaseSHA:     "base",
		EscalationGeneration: 1,
		EscalationCauses:     []remediationCause{remediationCauseSubstantive},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.addComment(9, spoofed)
	server.addCommentAs(9, "mallory", renderVerdictComment(apiv1.Verdict{
		Decision:  apiv1.VerdictFail,
		Rationale: noLanderEscalationPrefix + ` "fifo": human intervention is required`,
	}))

	set, err := electionIneligibleSet(context.Background(), server.newGitHubProvider("token"), repo, []providers.PullRequestSummary{
		{Number: 5, Base: "main", HeadSHA: "h5", BaseSHA: "base", Labels: []string{remediationEscalatedLabel}},
		{Number: 6, Base: "main", HeadSHA: "new-h6", BaseSHA: "base", Labels: []string{remediationEscalatedLabel}},
		{Number: 7, Base: "main", HeadSHA: "h7", BaseSHA: "base", Labels: []string{providers.LabelNeedsHuman}},
		{Number: 8, Base: "main", HeadSHA: "h8", BaseSHA: "base", Labels: []string{remediationEscalatedLabel}},
		{Number: 9, Base: "main", HeadSHA: "h9", BaseSHA: "base", Labels: []string{remediationEscalatedLabel}},
	})
	if err != nil {
		t.Fatalf("electionIneligibleSet: %v", err)
	}
	if !reflect.DeepEqual(set, map[int]bool{5: true, 7: true, 9: true}) {
		t.Fatalf("electionIneligibleSet = %v, want active escalations #5/#9 and needs-human #7", set)
	}
}

func TestElectionDrainsAroundIneligibleLander(t *testing.T) {
	findingsFor5 := []apiv1.Finding{blockedFinding(6, 7)}
	findingsFor6 := []apiv1.Finding{blockedFinding(5, 7)}
	ineligible := map[int]bool{5: true}

	if electionDecision(findingsFor5, 5, electedLander, ineligible) {
		t.Fatal("an ineligible lander must not retain the crown")
	}
	if !electionDecision(findingsFor6, 6, electedLander, ineligible) {
		t.Fatal("the next-lowest eligible PR must be crowned")
	}
	if got := predecessorBlockers(6, []int{5, 7}, electedLander, ineligible); len(got) != 0 {
		t.Fatalf("predecessorBlockers kept ineligible predecessor: %v", got)
	}
}
