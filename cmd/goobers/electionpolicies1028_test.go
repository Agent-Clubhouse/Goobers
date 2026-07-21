package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestScoredPolicy pins the shared comparator both cluster-data policies use:
// most-blockers elects the maximum score, fewest-overlaps the minimum, and a
// tie falls back to the fifo winner (lowest PR number) — so a scored policy is
// a total order and a drop-in for electionDecision/predecessorBlockers.
func TestScoredPolicy(t *testing.T) {
	scores := map[int]int{5: 1, 6: 3, 7: 2}

	most := scoredPolicy(scores, true) // most-blockers: higher wins
	if !most(6, []int{5, 7}) {
		t.Error("#6 (score 3, the max) should win most-blockers")
	}
	if most(5, []int{6, 7}) {
		t.Error("#5 (score 1) should not win most-blockers")
	}

	few := scoredPolicy(scores, false) // fewest-overlaps: lower wins
	if !few(5, []int{6, 7}) {
		t.Error("#5 (score 1, the min) should win fewest-overlaps")
	}
	if few(6, []int{5, 7}) {
		t.Error("#6 (score 3) should not win fewest-overlaps")
	}

	tie := scoredPolicy(map[int]int{5: 2, 6: 2, 7: 2}, true)
	if !tie(5, []int{6, 7}) {
		t.Error("a score tie must elect the lowest PR number (#5)")
	}
	if tie(6, []int{5, 7}) {
		t.Error("a score tie: #6 must not win over the lower-numbered #5")
	}
}

func TestGatherMostBlockersScores(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	provider := server.newGitHubProvider("token")

	mkPR := func(num int, blockers []int) providers.PullRequestSummary {
		server.addIssue(num, "pr")
		c, err := blockedOnSiblingComment(blockedOnSiblingState{Blockers: blockers})
		if err != nil {
			t.Fatalf("blockedOnSiblingComment: %v", err)
		}
		server.addComment(num, c)
		return providers.PullRequestSummary{Number: num, Labels: []string{blockedOnSiblingLabel}}
	}
	prs := []providers.PullRequestSummary{
		mkPR(20, []int{6}),
		mkPR(21, []int{6, 7}),
		mkPR(22, []int{6, 6}), // duplicate within one record counts once
		mkPR(23, []int{99}),   // off-cluster, ignored
	}

	scores, err := gatherMostBlockersScores(context.Background(), provider, repo, []int{5, 6, 7}, prs)
	if err != nil {
		t.Fatalf("gatherMostBlockersScores: %v", err)
	}
	for m, want := range map[int]int{5: 0, 6: 3, 7: 1} {
		if scores[m] != want {
			t.Errorf("score[#%d] = %d, want %d (%v)", m, scores[m], want, scores)
		}
	}
}

func TestGatherFewestOverlapsScores(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	provider := server.newGitHubProvider("token")

	// #5: {a,b}, #6: {b,c}, #7: {d}. #5 shares b with #6 (1); #6 shares b with
	// #5 (1); #7 shares nothing (0).
	server.addOpenPR(5, "h5", "main", "s5", "b1", false, nil, []fakePRFile{{path: "a.go"}, {path: "b.go"}})
	server.addOpenPR(6, "h6", "main", "s6", "b1", false, nil, []fakePRFile{{path: "b.go"}, {path: "c.go"}})
	server.addOpenPR(7, "h7", "main", "s7", "b1", false, nil, []fakePRFile{{path: "d.go"}})

	scores, err := gatherFewestOverlapsScores(context.Background(), provider, repo, []int{5, 6, 7})
	if err != nil {
		t.Fatalf("gatherFewestOverlapsScores: %v", err)
	}
	for m, want := range map[int]int{5: 1, 6: 1, 7: 0} {
		if scores[m] != want {
			t.Errorf("score[#%d] = %d, want %d (%v)", m, scores[m], want, scores)
		}
	}
}

// TestResolveElectionPolicyForCluster proves the static policies resolve without
// any gather (no provider dependency), the cluster-data policies elect the right
// member end to end, and an unknown name falls back to fifo.
func TestResolveElectionPolicyForCluster(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	ctx := context.Background()

	t.Run("fifo resolves statically", func(t *testing.T) {
		policy, name, err := resolveElectionPolicyForCluster(ctx, nil, repo, "fifo", 5, []int{6, 7}, nil)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if name != "fifo" || !policy(5, []int{6, 7}) || policy(6, []int{5, 7}) {
			t.Errorf("fifo should elect the lowest number without gathering")
		}
	})

	t.Run("unknown falls back to fifo", func(t *testing.T) {
		_, name, err := resolveElectionPolicyForCluster(ctx, nil, repo, "bogus", 5, []int{6}, nil)
		if err != nil || name != "fifo" {
			t.Errorf("unknown policy = (%q, %v), want fifo,nil", name, err)
		}
	})

	t.Run("most-blockers elects the max", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		provider := server.newGitHubProvider("token")
		mkPR := func(num int, blockers []int) providers.PullRequestSummary {
			server.addIssue(num, "pr")
			c, _ := blockedOnSiblingComment(blockedOnSiblingState{Blockers: blockers})
			server.addComment(num, c)
			return providers.PullRequestSummary{Number: num, Labels: []string{blockedOnSiblingLabel}}
		}
		// #6 is named by two open siblings, #5/#7 by none — so #6 wins even
		// though it is not the lowest number.
		prs := []providers.PullRequestSummary{mkPR(20, []int{6}), mkPR(21, []int{6})}
		policy, name, err := resolveElectionPolicyForCluster(ctx, provider, repo, policyMostBlockers, 5, []int{6, 7}, prs)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if name != policyMostBlockers {
			t.Fatalf("name = %q, want %q", name, policyMostBlockers)
		}
		if !policy(6, []int{5, 7}) {
			t.Error("#6 (named by the most siblings) should be crowned under most-blockers")
		}
		if policy(5, []int{6, 7}) {
			t.Error("#5 (the fifo winner) should NOT be crowned under most-blockers when #6 is named more")
		}
	})
}
