package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/goobers/goobers/providers"
)

// Cluster-data election policies (#1028/#1029), the deferred half of #834's
// pluggable-election seam. Unlike fifo/newest — pure functions of {PR number,
// blockers} — these score each cluster member from live cross-PR data and elect
// the extreme. They are OPT-IN (the shipped default stays fifo), so none of the
// gathering below runs unless a workflow sets electionPolicy to one of these;
// the fifo path is byte-identical to before.
const (
	policyMostBlockers   = "most-blockers"
	policyFewestOverlaps = "fewest-overlaps"
)

// isClusterDataPolicy reports whether name needs live cross-PR data (so the
// caller must gather it) rather than resolving to a pure static policy.
func isClusterDataPolicy(name string) bool {
	return name == policyMostBlockers || name == policyFewestOverlaps
}

// scoredPolicy builds an electionPolicyFunc from a precomputed per-PR score map:
// thisPR is the elected lander iff it ranks ahead of every PR it is blocked on.
// A total order over the cluster (score, then PR number as the fifo tiebreak)
// makes this a drop-in for the pure policies — both electionDecision (crowning)
// and predecessorBlockers (parking order) consume it, so the two stay
// consistent for these policies exactly as they do for fifo. higherWins picks
// the extreme: most-blockers elects the maximum score, fewest-overlaps the
// minimum.
func scoredPolicy(scores map[int]int, higherWins bool) electionPolicyFunc {
	ranksAhead := func(a, b int) bool {
		sa, sb := scores[a], scores[b]
		if sa != sb {
			if higherWins {
				return sa > sb
			}
			return sa < sb
		}
		return a < b // fifo tiebreak — matches electedLander when scores tie
	}
	return func(thisPR int, blockers []int) bool {
		for _, b := range blockers {
			if b == thisPR {
				continue
			}
			if !ranksAhead(thisPR, b) {
				return false
			}
		}
		return true
	}
}

// gatherMostBlockersScores counts, for each cluster member, how many still-open
// PRs name it as a blocker in their recorded blocked-on-sibling state (#748) —
// the "unblocks the most siblings" signal (#1028). Duplicate or off-cluster
// blocker references never inflate a member's count: each (namingPR, blocker)
// pair is seen once and only cluster members are scored.
func gatherMostBlockersScores(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, cluster []int, prs []providers.PullRequestSummary) (map[int]int, error) {
	inCluster := make(map[int]bool, len(cluster))
	for _, m := range cluster {
		inCluster[m] = true
	}
	scores := make(map[int]int, len(cluster))
	for _, m := range cluster {
		scores[m] = 0 // ensure every member is present, even at 0
	}
	for _, pr := range prs {
		blockers, err := recordedBlockedOnSiblingBlockers(ctx, provider, repo, pr)
		if err != nil {
			return nil, fmt.Errorf("read blocked-on-sibling record for pr #%d: %w", pr.Number, err)
		}
		seen := make(map[int]bool, len(blockers))
		for _, b := range blockers {
			if b == pr.Number || seen[b] || !inCluster[b] {
				continue
			}
			seen[b] = true
			scores[b]++
		}
	}
	return scores, nil
}

// gatherFewestOverlapsScores counts, for each cluster member, how many of its
// changed files at least one OTHER cluster member also changes (#1029) — the
// "smallest downstream reconcile surface" signal. A provider file-list failure
// is returned to the caller, which surfaces it through the stage's normal
// explicit failure path rather than silently changing the elected winner.
func gatherFewestOverlapsScores(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, cluster []int) (map[int]int, error) {
	files := make(map[int]map[string]bool, len(cluster))
	for _, m := range cluster {
		changed, err := provider.PullRequestFiles(ctx, repo, fmt.Sprintf("%d", m))
		if err != nil {
			return nil, fmt.Errorf("list files for pr #%d: %w", m, err)
		}
		set := make(map[string]bool, len(changed))
		for _, f := range changed {
			set[f.Path] = true
		}
		files[m] = set
	}
	scores := make(map[int]int, len(cluster))
	for _, m := range cluster {
		overlap := 0
		for path := range files[m] {
			for _, other := range cluster {
				if other == m {
					continue
				}
				if files[other][path] {
					overlap++
					break
				}
			}
		}
		scores[m] = overlap
	}
	return scores, nil
}

// resolveElectionPolicyForCluster resolves the configured policy to an
// electionPolicyFunc, gathering live cross-PR data for the cluster-data
// policies (#1028/#1029) and returning a pure static policy otherwise. The
// cluster is {selectedNumber} ∪ blockers. For a cluster-data policy a gather
// error is returned so the stage fails explicitly (never silently elects a
// different winner); an unknown name falls back to fifo, same as the pure
// resolver. Both elect-lander and apply-verdict call this with identical inputs
// and the same provider data, so they derive an identical policy — the two
// stages never disagree about who is crowned.
func resolveElectionPolicyForCluster(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, name string, selectedNumber int, blockers []int, prs []providers.PullRequestSummary) (electionPolicyFunc, string, error) {
	if !isClusterDataPolicy(name) {
		policy, resolved := resolveElectionPolicy(name)
		return policy, resolved, nil
	}
	cluster := append([]int{selectedNumber}, blockers...)
	sort.Ints(cluster)
	switch name {
	case policyMostBlockers:
		scores, err := gatherMostBlockersScores(ctx, provider, repo, cluster, prs)
		if err != nil {
			return nil, name, err
		}
		return scoredPolicy(scores, true), name, nil
	case policyFewestOverlaps:
		scores, err := gatherFewestOverlapsScores(ctx, provider, repo, cluster)
		if err != nil {
			return nil, name, err
		}
		return scoredPolicy(scores, false), name, nil
	default:
		policy, resolved := resolveElectionPolicy(name)
		return policy, resolved, nil
	}
}
