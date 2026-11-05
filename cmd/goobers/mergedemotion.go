package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func mergeDemotionComment(s mergeDemotionState) (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal merge-demotion payload: %w", err)
	}
	return fmt.Sprintf("<!-- merge-demotion-state: %s -->", data), nil
}

// demotedSet returns the subset of the given open PRs that are currently
// demoted, snapshot-validated. Callers already hold the open-PR list (pr-select
// and elect-lander both list it), so this adds only a ListComments per
// currently-labeled PR. Per #950's fail-safe contract the CALLER decides what
// to do with an error: the election read-sites treat a resolution failure as an
// empty demoted set (today's behavior) rather than failing the pipeline, so a
// provider hiccup can never turn the demotion signal into a merge outage.
func demotedSet(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prs []providers.PullRequestSummary) (map[int]bool, error) {
	out := map[int]bool{}
	for _, pr := range prs {
		if !hasAnyLabel(pr.Labels, []string{mergeDemotedLabel}) {
			continue
		}
		held, err := demotionStillHolds(ctx, provider, repo, pr)
		if err != nil {
			return nil, err
		}
		if held {
			out[pr.Number] = true
		}
	}
	return out, nil
}

// electionIneligibleSet returns open PRs that are currently parked outside the
// autonomous landing loop. Active escalations are snapshot-validated so a
// self-healed PR immediately becomes eligible again. This includes no-lander
// escalations: retaining an ineligible deterministic winner in the candidate
// set prevents every healthy sibling from landing and makes the requested
// human intervention a cluster-wide deadlock rather than a hold on one PR.
func electionIneligibleSet(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prs []providers.PullRequestSummary) (map[int]bool, error) {
	out := map[int]bool{}
	for _, pr := range prs {
		deferred, err := noLanderDeferralStillHolds(ctx, provider, repo, pr)
		if err != nil {
			return nil, err
		}
		if deferred {
			out[pr.Number] = true
			continue
		}
		if hasAnyLabel(pr.Labels, []string{providers.LabelNeedsHuman}) {
			out[pr.Number] = true
			continue
		}
		if !hasAnyLabel(pr.Labels, []string{remediationEscalatedLabel}) {
			continue
		}
		blocked, err := escalationStillBlocks(ctx, provider, repo, pr)
		if err != nil {
			return nil, err
		}
		if !blocked {
			continue
		}
		out[pr.Number] = true
	}
	return out, nil
}

// noLanderDeferralStillHolds excludes only the current, trusted election
// disposition. A changed head/base permits a new election, and prose or an
// untrusted comment cannot remove a candidate from the cluster.
func noLanderDeferralStillHolds(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	if !hasAnyLabel(pr.Labels, []string{blockedOnSiblingLabel}) {
		return false, nil
	}
	author, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return false, err
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		if !isTrustedMergeReviewStatusComment(comment.Author, comment.Body, author) {
			continue
		}
		verdict, ok := parseVerdictComment(comment.Body)
		if !ok {
			return false, nil
		}
		return verdict.Decision == apiv1.VerdictDefer && verdict.ReasonCode == apiv1.VerdictReasonNoLander &&
			verdict.HeadSHA != "" && verdict.HeadSHA == pr.HeadSHA &&
			verdict.BaseSHA != "" && verdict.BaseSHA == pr.BaseSHA, nil
	}
	return false, nil
}

func unionPRSets(sets ...map[int]bool) map[int]bool {
	out := map[int]bool{}
	for _, set := range sets {
		for number := range set {
			out[number] = true
		}
	}
	return out
}

// withoutDemoted removes any PR excluded by demotion or current ineligibility
// from a blocker list. Both election read-sites (elect-lander's electionDecision
// and apply-verdict's predecessorBlockers/electedLanderPass) apply it identically
// so the two stages never disagree about which sibling a PR must wait behind.
func withoutDemoted(blockers []int, demoted map[int]bool) []int {
	if len(demoted) == 0 {
		return blockers
	}
	out := make([]int, 0, len(blockers))
	for _, b := range blockers {
		if demoted[b] {
			continue
		}
		out = append(out, b)
	}
	return out
}
