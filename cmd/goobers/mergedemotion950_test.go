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
