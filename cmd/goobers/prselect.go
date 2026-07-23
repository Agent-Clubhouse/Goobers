package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/providers"
)

// defaultExcludeLabels are the labels that mean "already decided, don't
// re-review" (design doc §3): a PR merge-review already verdicted this cycle
// carries one of these until pr-remediation/auto-merge acts on it and clears
// it. Re-selecting it would waste a cycle re-reviewing something already in
// flight — harmless under G3, but pointless.
//
// goobers:merge-escalated is deliberately NOT a static entry here (#716): a
// permanent label-based exclusion can never self-heal once a sibling merge
// or new commits change the PR's actual situation. It is instead checked via
// escalationStillBlocks below, which compares the PR's current head/base
// against the snapshot recorded at escalation time.
const defaultExcludeLabels = "goobers:merge-ready,goobers:needs-remediation"

// runPRSelect implements `goobers pr-select` (issues #359 and #481):
// merge-review's selection stage. Picks at most one eligible PR per run — the same
// one-per-run shape backlog-query uses for issues (design doc §3's
// declarative-selection model), not a batch scan of the whole open-PR set in
// a single run. The selected PR is leased in the shared PR claim namespace so
// concurrent merge-review and pr-remediation runs cannot select it together.
const prSelectHelp = "Usage: goobers pr-select [path]\n\n" +
	"Select at most one open, non-draft, green-CI goober-authored PR for\n" +
	"merge-review to evaluate this cycle (a workflow stage). Before selection,\n" +
	"park narrower PRs behind open PRs that clearly dominate a shared-file\n" +
	"rewrite or deletion. Writes the\n" +
	"selected PR's number/head/base/headSha/baseSha/url to the declared\n" +
	"result file. Exit codes: 0 = selected (or no-work), 1 = business error,\n" +
	"2 = usage/IO error.\n"

func runPRSelect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pr-select", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "pr-select")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newCachedGitHubProvider(root, token)

	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", providerBranchNamespace()+"implementation/")
	excludeLabels := splitLabelList(providerInput("excludeLabels", defaultExcludeLabels))

	ctx, cancel := providerCommandContext()
	defer cancel()
	now := time.Now().UTC()
	triggerRef := os.Getenv(executor.TriggerRefEnvVar)
	completeness, err := prSelectSnapshotCompletenessForRun(root, repo, triggerRef, now)
	if err != nil {
		pf(stderr, "error: determine PR snapshot completeness: %v\n", err)
		return 1
	}
	prs, err := pullRequestsForSelection(ctx, provider, repo, base, headPrefix, triggerRef, completeness)
	if err != nil {
		return failProviderStage(stderr, "load pull requests", err, "selected-pr.json")
	}

	blockerScanCtx, cancelBlockerScan := blockedOnSiblingScanContext(ctx)
	defer cancelBlockerScan()
	siblingBlocked := make(map[int]bool)
	liveSiblingBlockers := make(map[int][]int)
	blockedDependents := make(map[int]int)
	for _, pr := range prs {
		blockers, err := liveBlockedOnSiblingBlockers(blockerScanCtx, provider, repo, pr)
		if err != nil {
			return failProviderStage(stderr, fmt.Sprintf("check blocked-on-sibling state for PR #%d", pr.Number), err, "selected-pr.json")
		}
		liveSiblingBlockers[pr.Number] = blockers
		siblingBlocked[pr.Number] = len(blockers) > 0
		for _, blocker := range blockers {
			blockedDependents[blocker]++
		}
	}
	couplings, err := loadFoundationCouplings(blockerScanCtx, provider, repo, prs, siblingBlocked)
	if err != nil {
		return failProviderStage(stderr, "detect foundation-coupled pull requests", err, "selected-pr.json")
	}
	for _, coupling := range couplings {
		changed, ferr := flagFoundationCoupling(
			blockerScanCtx, provider, repo, coupling, liveSiblingBlockers[coupling.dependent.Number],
		)
		if ferr != nil {
			return failProviderStage(stderr, fmt.Sprintf("flag foundation-coupled PR #%d", coupling.dependent.Number), ferr, "selected-pr.json")
		}
		if !changed {
			continue
		}
		liveSiblingBlockers[coupling.dependent.Number] = append(
			liveSiblingBlockers[coupling.dependent.Number], coupling.foundation.Number,
		)
		siblingBlocked[coupling.dependent.Number] = true
		blockedDependents[coupling.foundation.Number]++
		pf(stdout, "foundation-coupled: parked PR #%d behind PR #%d (%s)\n",
			coupling.dependent.Number, coupling.foundation.Number, strings.Join(coupling.files, ", "))
	}

	var eligible []providers.PullRequestSummary
	for _, pr := range prs {
		if pr.State != "open" || pr.Base != base || !strings.HasPrefix(pr.Head, headPrefix) {
			continue
		}
		if pr.Draft {
			continue
		}
		if pr.CheckState != providers.CheckStatePassing {
			continue
		}
		if hasAnyLabel(pr.Labels, excludeLabels) {
			continue
		}
		blocked, err := escalationStillBlocks(ctx, provider, repo, pr)
		if err != nil {
			return failProviderStage(stderr, fmt.Sprintf("check escalation state for PR #%d", pr.Number), err, "selected-pr.json")
		}
		if blocked {
			continue
		}
		// #950: a demoted PR (repeatedly could not merge at an unchanged head)
		// is excluded from selection so the election stops re-crowning the stuck
		// lander; its cluster drains around it via the blocked-on-sibling
		// liveness change. Self-heals the instant its head advances, same as
		// escalationStillBlocks above. Fail OPEN — treat a resolution error as
		// not-demoted (today's behavior) so the demotion signal can never itself
		// keep an otherwise-eligible PR out of merge-review.
		demoted, derr := demotionStillHolds(ctx, provider, repo, pr)
		if derr != nil {
			pf(stderr, "warning: could not resolve merge-demotion state for PR #%d (%v) — treating as not demoted\n", pr.Number, derr)
			demoted = false
		}
		if demoted {
			continue
		}
		// #748: a PR parked goobers:blocked-on-sibling is skipped while any of
		// its named blocker PRs is still open — re-reviewing it would just
		// reproduce the identical cross-PR verdict. Self-heals (selectable
		// again) automatically once every blocker merges or closes, with no
		// human clearing the label.
		if siblingBlocked[pr.Number] {
			continue
		}
		eligible = append(eligible, pr)
	}
	observation, err := observePRSelectEligibility(root, repo, prs, eligible, completeness, now)
	if err != nil {
		pf(stderr, "error: update PR fairness state: %v\n", err)
		return 1
	}
	if len(eligible) == 0 {
		return writeNoWorkResult(stdout, stderr, "no eligible PR to select this cycle")
	}
	eligible, priorities, fairness := rankEligiblePullRequests(
		observation.UnclaimedEligible, blockedDependents, observation.EligibleSince, now,
	)
	if observation.CurrentRunHasLiveClaim {
		if len(observation.CurrentRunClaimEligible) == 0 {
			return writeNoWorkResult(stdout, stderr, "current run already holds a live claim outside the eligible snapshot")
		}
		eligible, priorities, _ = rankEligiblePullRequests(
			observation.CurrentRunClaimEligible, blockedDependents, nil, now,
		)
	}
	if len(eligible) == 0 {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}

	claimed, err := claimEligiblePullRequestInOrder(root, eligible)
	if err != nil {
		pf(stderr, "error: claim eligible PR: %v\n", err)
		return 1
	}
	if claimed == nil {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}
	selected := *claimed
	if err := clearPRSelectEligibilityWait(root, repo, selected); err != nil {
		pf(stderr, "error: clear selected PR fairness state: %v\n", err)
		return 1
	}
	priority := priorities[selected.Number]

	resultFile := providerInput("resultFile", "selected-pr.json")
	data, err := json.Marshal(map[string]string{
		"number":                 strconv.Itoa(selected.Number),
		"head":                   selected.Head,
		"base":                   selected.Base,
		"headSha":                selected.HeadSHA,
		"baseSha":                selected.BaseSHA,
		"url":                    selected.URL,
		"eligibleSince":          priority.EligibleSince.Format(time.RFC3339Nano),
		"eligibleWaitSeconds":    strconv.FormatInt(int64(priority.Wait/time.Second), 10),
		"agingBoost":             strconv.FormatInt(priority.AgingBoost, 10),
		"starvationGuarded":      strconv.FormatBool(priority.StarvationGuarded),
		"maxEligibleWaitSeconds": strconv.FormatInt(int64(fairness.MaxWait/time.Second), 10),
		"starvedEligiblePRsCsv":  joinPRNumbers(fairness.Starved),
	})
	if err != nil {
		pf(stderr, "error: marshal selected PR: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "selected PR #%d: %s\n", selected.Number, selected.URL)
	pf(stdout, "selection fairness: eligible wait %s, max eligible wait %s, starvation guard %t, starved eligible PRs %s\n",
		priority.Wait.Round(time.Second),
		fairness.MaxWait.Round(time.Second),
		priority.StarvationGuarded,
		noneIfEmpty(joinPRNumbers(fairness.Starved)),
	)
	return 0
}

func pullRequestsForSelection(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	base string,
	headPrefix string,
	triggerRef string,
	completeness prSelectSnapshotCompleteness,
) ([]providers.PullRequestSummary, error) {
	pullID, targeted := webhookhttp.PullNumberFromTriggerRef(triggerRef)
	if !targeted || completeness == prSelectCompleteSnapshot {
		return provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
			Repository: repo, Base: base, HeadPrefix: headPrefix,
		})
	}
	pr, err := provider.GetPullRequest(ctx, repo, pullID)
	if err != nil {
		return nil, fmt.Errorf("read webhook pull request #%s: %w", pullID, err)
	}
	pr.CheckState, err = provider.RefCheckState(ctx, repo, pr.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("read webhook pull request #%s checks: %w", pullID, err)
	}
	return []providers.PullRequestSummary{pr}, nil
}

func splitLabelList(value string) []string {
	var labels []string
	for _, label := range strings.Split(value, ",") {
		if label = strings.TrimSpace(label); label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

// hasAnyLabel reports whether labels contains any of wants (case-sensitive,
// matching GitHub's own label-name comparison).
func hasAnyLabel(labels, wants []string) bool {
	for _, w := range wants {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		for _, l := range labels {
			if l == w {
				return true
			}
		}
	}
	return false
}
