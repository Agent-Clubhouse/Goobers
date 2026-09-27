package main

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

// Landing scope (#5602). merge-review sequences a managed PR against its
// overlapping siblings, and FIFO election crowns one lander per cluster. Both
// are only live if every PR a managed selection can wait behind is one this
// instance can itself land: select (pr-select's ownership test), elect, and
// merge. Before #5602 the sibling set was the whole branch namespace
// (`goobers/`) while selection was the narrower headPrefixes
// (`goobers/implementation/`), so a PR in the namespace but outside
// headPrefixes — left by another instance configuration — became a sequencing
// blocker that could only ever receive an advisory review. Every managed PR
// in its overlap cluster deferred behind it, and the cluster parked until a
// human closed it. A `goobers:no-merge-review` PR had the same shape: never
// selected, still a blocker.
//
// Excluding such a PR from sequencing cannot produce an unsafe merge. This
// instance never merges it, so the only question is which of the two lands
// first, and the one we cannot land has no turn to wait for. The managed PR
// landing first is exactly what would happen if the other one were merged by
// hand later: its owner rebases it, as for any other base advance. File
// overlap between two PRs is a sequencing concern, not a correctness one —
// each merge still goes through the provider's own conflict and CI checks.

// mergeReviewCanLand reports whether pr is one this merge-review instance may
// select, elect and merge: its own under the same ownership test pr-select
// uses, and not opted out of merge-review.
func mergeReviewCanLand(pr providers.PullRequestSummary, headPrefixes []string, expectedAuthorLogin string) bool {
	return isOwnPullRequest(pr.Author, pr.Head, headPrefixes, expectedAuthorLogin) &&
		!hasAnyLabel(pr.Labels, []string{noMergeReviewLabel})
}

// unlandableSiblingSet returns the open PRs under the branch namespace that
// this instance cannot land. Only namespace PRs are named: they are the ones
// sequencing previously treated as managed siblings, so this is exactly the
// set #5602 stops waiting behind. PRs outside the namespace keep their
// existing treatment.
func unlandableSiblingSet(prs []providers.PullRequestSummary, headPrefixes []string, expectedAuthorLogin string) map[int]bool {
	namespace := providerBranchNamespace()
	out := map[int]bool{}
	for _, pr := range prs {
		if strings.HasPrefix(pr.Head, namespace) && !mergeReviewCanLand(pr, headPrefixes, expectedAuthorLogin) {
			out[pr.Number] = true
		}
	}
	return out
}

// admitsManagedSibling reports whether a managed selection sequences against
// pr, and records a namespace PR it declines as unlandable so the election
// stages can drop it from any blocker set the reviewer still names.
func (s *siblingOwnershipScope) admitsManagedSibling(pr providers.PullRequestSummary) bool {
	if mergeReviewCanLand(pr, s.headPrefixes, s.expectedAuthorLogin) {
		return true
	}
	if strings.HasPrefix(pr.Head, s.managedHeadPrefix) {
		s.unlandable = append(s.unlandable, strconv.Itoa(pr.Number))
	}
	return false
}

// electionExcludedSet resolves every open PR the election drops from
// candidacy and from each sibling's blocker set: snapshot-valid demotions
// (#950), PRs parked outside the landing loop, and PRs this instance cannot
// land (#5602). elect-lander and apply-verdict both call it with the same
// inputs, so the two stages agree on the crown and on the recorded
// predecessors. unlandableCsv is gather-sibling-context's unlandableSiblingsCsv;
// a workflow without that edge supplies "", which is the pre-#5602 behavior.
// A demotion lookup failure degrades to no demotions, per #950's
// fail-safe contract; an eligibility lookup failure is returned.
func electionExcludedSet(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prs []providers.PullRequestSummary, unlandableCsv string, stderr io.Writer) (map[int]bool, error) {
	demoted, err := demotedSet(ctx, provider, repo, prs)
	if err != nil {
		pf(stderr, "warning: could not resolve merge-demotion state (%v) — proceeding without it\n", err)
		demoted = nil
	}
	// The FIFO lander election (#950) is a GitHub merge-queue concept with no
	// Gitea equivalent; skip it on other forges rather than fail closed.
	if githubProvider, ok := provider.(*providers.GitHubProvider); ok {
		ineligible, err := electionIneligibleSet(ctx, githubProvider, repo, prs)
		if err != nil {
			return nil, err
		}
		demoted = unionPRSets(demoted, ineligible)
	}
	unlandable := map[int]bool{}
	for _, number := range parseOverlappingSiblings(unlandableCsv) {
		unlandable[number] = true
	}
	return unionPRSets(demoted, unlandable), nil
}
