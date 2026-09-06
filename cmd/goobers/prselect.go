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
	"github.com/goobers/goobers/internal/instance"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/providers"
)

// defaultExcludeLabels are the labels that mean "already decided, don't
// re-review" (design doc §3): a PR merge-review already verdicted this cycle
// carries one of these until pr-remediation/auto-merge acts on it and clears
// it. The acknowledged scope-gate exception below admits one refresh cycle
// because the operator action changes the gate outcome without moving the PR.
//
// goobers:merge-escalated is deliberately NOT a static entry here (#716): a
// permanent label-based exclusion can never self-heal once a sibling merge
// or new commits change the PR's actual situation. It is instead checked via
// escalationStillBlocks below, which compares the PR's current head/base
// against the snapshot recorded at escalation time.
const (
	defaultExcludeLabels = "goobers:merge-ready,goobers:needs-remediation"
	noMergeReviewLabel   = "goobers:no-merge-review"
	authorScopeGoobers   = "goobers"
	authorScopeAny       = "any"
)

func defaultMergeReviewHeadPrefix() string {
	return providerBranchNamespace() + "implementation/"
}

func mergeReviewHeadPrefixes() []string {
	raw := providerInput("headPrefixes", "")
	if strings.TrimSpace(raw) == "" {
		return []string{providerInput("headPrefix", defaultMergeReviewHeadPrefix())}
	}
	return splitLabelList(raw)
}

func mergeReviewCheckStateEligible(state providers.CheckState, allowPending bool) bool {
	return state == providers.CheckStatePassing ||
		(allowPending && state == providers.CheckStatePending)
}

// runPRSelect implements `goobers pr-select` (issues #359 and #481):
// merge-review's selection stage. Picks at most one eligible PR per run — the same
// one-per-run shape backlog-query uses for issues (design doc §3's
// declarative-selection model), not a batch scan of the whole open-PR set in
// a single run. The selected PR is leased in the shared PR claim namespace so
// concurrent merge-review and pr-remediation runs cannot select it together.
const prSelectHelp = "Usage: goobers pr-select [path]\n\n" +
	"Select at most one open, non-draft PR for merge-review to evaluate this\n" +
	"cycle (a workflow stage). CI must be passing unless allowPendingChecks is\n" +
	"true; known failing and unknown states are never eligible. authorScope\n" +
	"defaults to goobers;\n" +
	"set it to any to admit PRs outside headPrefixes as advisory-only. PRs\n" +
	"may be filtered by exact author, assignee, and requestedReviewer inputs.\n" +
	"PRs labeled goobers:no-merge-review or goobers:run-aborted are always\n" +
	"excluded. Before selection,\n" +
	"park narrower PRs behind open PRs that clearly dominate a shared-file\n" +
	"rewrite or deletion. Writes the\n" +
	"selected PR's number/head/base/headSha/baseSha/url/advisoryMode to the declared\n" +
	"result file. Exit codes: 0 = selected (or no-work), 1 = business error,\n" +
	"2 = usage/IO error.\n"

func runPRSelect(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("pr-select", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "pr-select")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	source, gateProvider, err := newPRSelectSources(root, repo)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return runPRSelectCore(root, repo, source, gateProvider, stdout, stderr)
}

// runPRSelectCore owns the provider-neutral selection policy. source supplies
// the current PR snapshot through the provider's supported read channel;
// gateProvider is present only for the issue-comment forges, whose
// sibling/foundation/escalation/demotion/Tutor gates are supported.
func runPRSelectCore(
	root string,
	repo providers.RepositoryRef,
	source prSelectSource,
	gateProvider remediationProvider,
	stdout, stderr io.Writer,
) int {

	base := providerInput("base", providerBaseBranch())
	headPrefixes := mergeReviewHeadPrefixes()
	authorScope := providerInput("authorScope", authorScopeGoobers)
	if authorScope != authorScopeGoobers && authorScope != authorScopeAny {
		pf(stderr, "error: authorScope input %q must be %q or %q\n", authorScope, authorScopeGoobers, authorScopeAny)
		return 1
	}
	excludeLabels := splitLabelList(providerInput("excludeLabels", defaultExcludeLabels))
	allowPendingChecks, err := strconv.ParseBool(providerInput("allowPendingChecks", "false"))
	if err != nil {
		pf(stderr, "error: allowPendingChecks must be true or false\n")
		return 1
	}
	// abortedRunLabel and LabelNeedsHuman are always excluded, never
	// operator-overridable via the excludeLabels input, same as
	// noMergeReviewLabel: a cancelled run's PR must stay ineligible for
	// auto-merge until a human removes the label directly (#2238).
	// LabelNeedsHuman mirrors the exclusion pr-remediation's
	// filterRemediationPullRequests and backlog-query's re-sweep filter
	// already apply: the #2947 failure-streak circuit breaker applies this
	// label after repeated terminal failures on the same claimed item, and
	// until #3262 it wasn't checked here, so pr-select kept reselecting a PR
	// the breaker had already tried to park (#3262).
	excludeLabels = append(excludeLabels, noMergeReviewLabel, abortedRunLabel, providers.LabelNeedsHuman)
	identityFilters := providers.ListPullRequestsRequest{
		Author:            providerInput("author", ""),
		Assignee:          providerInput("assignee", ""),
		RequestedReviewer: providerInput("requestedReviewer", ""),
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	requiredOptInLabel := strings.TrimSpace(providerInput("requireOptInLabel", ""))
	respectAssignee, err := strconv.ParseBool(providerInput("respectAssignee", "false"))
	if err != nil {
		pf(stderr, "error: invalid respectAssignee input: %v\n", err)
		return 1
	}
	selfIdentity := strings.TrimSpace(providerInput("selfIdentity", ""))
	if respectAssignee && selfIdentity == "" {
		identitySource, ok := source.(prSelectSelfIdentitySource)
		if !ok {
			pf(stderr, "error: selfIdentity is required for respectAssignee on Azure DevOps\n")
			return 1
		}
		selfIdentity, err = identitySource.resolveSelfIdentity(ctx)
		if err != nil {
			pf(stderr, "error: resolve selfIdentity for assignee policy: %v\n", err)
			return 1
		}
	}
	now := time.Now().UTC()
	expectedAuthorLogin := source.expectedAuthorLogin(ctx, root)
	triggerRef := os.Getenv(executor.TriggerRefEnvVar)
	completeness, err := prSelectSnapshotCompletenessForRun(root, repo, triggerRef, now)
	if err != nil {
		pf(stderr, "error: determine PR snapshot completeness: %v\n", err)
		return 1
	}
	prs, openPRs, err := source.pullRequests(ctx, repo, prSelectSourceRequest{
		base:                base,
		headPrefixes:        headPrefixes,
		authorScope:         authorScope,
		identityFilters:     identityFilters,
		triggerRef:          triggerRef,
		completeness:        completeness,
		expectedAuthorLogin: expectedAuthorLogin,
	})
	if err != nil {
		return failProviderStage(stderr, "load pull requests", err, "selected-pr.json")
	}

	gateState, gateCode := loadPRSelectSafetyGateState(
		ctx, gateProvider, repo, openPRs, base, headPrefixes, expectedAuthorLogin, stdout, stderr,
	)
	if gateCode != 0 {
		return gateCode
	}

	var eligible []providers.PullRequestSummary
	// Goobers#4177: an advisory cycle is terminal and read-only, so nothing
	// it does makes its subject ineligible next tick. Suppress a head SHA
	// that has already been advised, or the oldest advisory PR wins ranking
	// forever and starves every managed PR behind it.
	advisedHeads, err := loadPRSelectAdvisedHeads(root, repo, now)
	if err != nil {
		pf(stderr, "error: read advisory suppression state: %v\n", err)
		return 1
	}
	for _, pr := range prs {
		if pr.State != "open" || pr.Base != base ||
			(authorScope != authorScopeAny && !isOwnPullRequest(pr.Author, pr.Head, headPrefixes, expectedAuthorLogin)) {
			continue
		}
		if pr.Draft {
			continue
		}
		if !mergeReviewCheckStateEligible(pr.CheckState, allowPendingChecks) {
			continue
		}
		if hasPRSelectExclusion(pr.Labels, excludeLabels) {
			continue
		}

		if authorScope == authorScopeAny &&
			!isOwnPullRequest(pr.Author, pr.Head, headPrefixes, expectedAuthorLogin) &&
			advisoryAlreadyDispatched(advisedHeads, pr) {
			pf(stdout, "skipped PR #%d: advisory verdict already published for head %s\n",
				pr.Number, shortBaselineSHA(pr.HeadSHA))
			continue
		}
		if !eligibleByMergeReviewPolicy(pr, requiredOptInLabel, respectAssignee, selfIdentity) {
			pf(stdout, "rejected PR #%d by merge-review eligibility policy: %s\n", pr.Number,
				mergeReviewPolicyRejection(pr, requiredOptInLabel, respectAssignee, selfIdentity))
			continue
		}
		blocked, gateCode := prSelectSafetyGatesBlock(
			ctx, gateProvider, repo, pr, gateState.siblingBlocked[pr.Number], stdout, stderr,
		)
		if gateCode != 0 {
			return gateCode
		}
		if blocked {
			continue
		}
		eligible = append(eligible, pr)
	}
	return completePRSelection(root, repo, prs, eligible, completeness, now,
		gateState.blockedDependents, triggerRef, authorScope, headPrefixes, expectedAuthorLogin,
		requiredOptInLabel, respectAssignee, selfIdentity, stdout, stderr)
}

// completePRSelection is the provider-neutral selection decision after a
// source has supplied its eligible snapshot. Fairness, target narrowing,
// claiming, and the result contract are identical for every provider.
func completePRSelection(
	root string,
	repo providers.RepositoryRef,
	prs, eligible []providers.PullRequestSummary,
	completeness prSelectSnapshotCompleteness,
	now time.Time,
	blockedDependents map[int]int,
	triggerRef, authorScope string,
	headPrefixes []string,
	expectedAuthorLogin, requiredOptInLabel string,
	respectAssignee bool,
	selfIdentity string,
	stdout, stderr io.Writer,
) int {
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
	eligible = restrictSelectionToTargetedPullRequest(eligible, triggerRef)
	if observation.CurrentRunHasLiveClaim {
		if len(observation.CurrentRunClaimEligible) == 0 {
			return writeNoWorkResult(stdout, stderr, "current run already holds a live claim outside the eligible snapshot")
		}
		eligible, priorities, _ = rankEligiblePullRequests(
			observation.CurrentRunClaimEligible, blockedDependents, nil, now,
		)
		eligible = restrictSelectionToTargetedPullRequest(eligible, triggerRef)
	}
	if len(eligible) == 0 {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}

	claimed, err := claimEligiblePullRequestInOrder(root, repo, eligible)
	if err != nil {
		pf(stderr, "error: claim eligible PR: %v\n", err)
		return 1
	}
	if claimed == nil {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}
	selected := *claimed
	advisoryMode := authorScope == authorScopeAny && !isOwnPullRequest(selected.Author, selected.Head, headPrefixes, expectedAuthorLogin)
	if advisoryMode {
		if err := recordPRSelectAdvisory(root, repo, selected, now); err != nil {
			pf(stderr, "error: record advisory selection: %v\n", err)
			return 1
		}
	}
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
		"advisoryMode":           strconv.FormatBool(advisoryMode),
		"eligibleSince":          priority.EligibleSince.Format(time.RFC3339Nano),
		"eligibleWaitSeconds":    strconv.FormatInt(int64(priority.Wait/time.Second), 10),
		"agingBoost":             strconv.FormatInt(priority.AgingBoost, 10),
		"starvationGuarded":      strconv.FormatBool(priority.StarvationGuarded),
		"maxEligibleWaitSeconds": strconv.FormatInt(int64(fairness.MaxWait/time.Second), 10),
		"starvedEligiblePRsCsv":  joinPRNumbers(fairness.Starved),
		"eligibilityPolicy":      mergeReviewEligibilityDescription(requiredOptInLabel, respectAssignee, selfIdentity),
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
	pf(stdout, "selection eligibility policy: %s\n", mergeReviewEligibilityDescription(requiredOptInLabel, respectAssignee, selfIdentity))
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
	provider remediationProvider,
	repo providers.RepositoryRef,
	base string,
	headPrefixes []string,
	authorScope string,
	identityFilters providers.ListPullRequestsRequest,
	triggerRef string,
	completeness prSelectSnapshotCompleteness,
	expectedAuthorLogin string,
) ([]providers.PullRequestSummary, []providers.PullRequestSummary, error) {
	openPRs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, SkipCheckState: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list open pull requests: %w", err)
	}
	pullID, targeted := webhookhttp.PullNumberFromTriggerRef(triggerRef)
	if targeted && completeness != prSelectCompleteSnapshot {
		pr, err := provider.GetPullRequest(ctx, repo, pullID)
		if err != nil {
			return nil, nil, fmt.Errorf("read webhook pull request #%s: %w", pullID, err)
		}
		if !identityFilters.MatchesIdentityFields(pr.Author, pr.Assignees, pr.RequestedReviewers) {
			return nil, openPRs, nil
		}
		pr.CheckState, err = provider.RefCheckState(ctx, repo, pr.HeadSHA)
		if err != nil {
			return nil, nil, fmt.Errorf("read webhook pull request #%s checks: %w", pullID, err)
		}
		return []providers.PullRequestSummary{pr}, openPRs, nil
	}

	prs := make([]providers.PullRequestSummary, 0, len(openPRs))
	for _, pr := range openPRs {
		if authorScope != authorScopeAny && !isOwnPullRequest(pr.Author, pr.Head, headPrefixes, expectedAuthorLogin) {
			continue
		}
		if !identityFilters.MatchesIdentityFields(pr.Author, pr.Assignees, pr.RequestedReviewers) {
			continue
		}
		pr.CheckState, err = provider.RefCheckState(ctx, repo, pr.HeadSHA)
		if err != nil {
			return nil, nil, fmt.Errorf("read pull request #%d checks: %w", pr.Number, err)
		}
		prs = append(prs, pr)
	}
	return prs, openPRs, nil
}

// prSelectSource contains only the provider-dependent reads that construct the
// selection snapshot. The decision core deliberately keeps eligibility,
// fairness, claims, and result handling outside this adapter.
type prSelectSource interface {
	pullRequests(context.Context, providers.RepositoryRef, prSelectSourceRequest) ([]providers.PullRequestSummary, []providers.PullRequestSummary, error)
	expectedAuthorLogin(context.Context, string) string
}

// prSelectSelfIdentitySource is implemented only where the configured
// credential exposes a stable PR assignee/reviewer identity.
type prSelectSelfIdentitySource interface {
	prSelectSource
	resolveSelfIdentity(context.Context) (string, error)
}

type prSelectSourceRequest struct {
	base                string
	headPrefixes        []string
	authorScope         string
	identityFilters     providers.ListPullRequestsRequest
	triggerRef          string
	completeness        prSelectSnapshotCompleteness
	expectedAuthorLogin string
}

// newPRSelectSources selects the supported snapshot channel. GitHub and Gitea
// use the normal single-PR/check-ref source; ADO uses its branch-policy poll
// source and intentionally supplies no remediation gate provider. That makes
// the documented ADO no-op gates unreachable rather than probing unsupported
// comment, file, or branch operations.
func newPRSelectSources(root string, repo providers.RepositoryRef) (prSelectSource, remediationProvider, error) {
	if repo.Provider == providers.ProviderADO {
		provider, err := newMergeReviewProvider(root, repo, true)
		if err != nil {
			return nil, nil, err
		}
		return branchPolicyPRSelectSource{provider: providers.NewDispatcher(provider)}, nil, nil
	}

	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return nil, nil, err
	}
	// Dispatch on the routed repo's own provider kind. Constructing a GitHub
	// provider unconditionally addressed a Gitea-routed repo's selection scan
	// to api.github.com with a Gitea credential, failing the stage with a 401
	// github_auth_failed on a repo that has no GitHub side at all — the same
	// defect open-pr's per-kind dispatch fixed for PR creation.
	provider, err := remediationStageProvider(root, repo, token, true)
	if err != nil {
		return nil, nil, err
	}
	return refCheckPRSelectSource{provider: provider}, provider, nil
}

// refCheckPRSelectSource is the GitHub/Gitea source: checks are resolved at a
// head ref and a webhook-targeted PR is read directly.
type refCheckPRSelectSource struct {
	provider remediationProvider
}

func (s refCheckPRSelectSource) pullRequests(ctx context.Context, repo providers.RepositoryRef, request prSelectSourceRequest) ([]providers.PullRequestSummary, []providers.PullRequestSummary, error) {
	return pullRequestsForSelection(
		ctx, s.provider, repo, request.base, request.headPrefixes, request.authorScope,
		request.identityFilters, request.triggerRef, request.completeness, request.expectedAuthorLogin,
	)
}

func (s refCheckPRSelectSource) resolveSelfIdentity(ctx context.Context) (string, error) {
	return s.provider.AuthenticatedLogin(ctx)
}

func (s refCheckPRSelectSource) expectedAuthorLogin(ctx context.Context, root string) string {
	return daemonIdentityAuthorLogin(ctx, root, s.provider)
}

// branchPolicyPRSelectSource is the ADO source. Its policy evaluations are the
// source of truth for CheckState, and it intentionally has no inferred
// self-identity or daemon-login lookup: ADO selection stays on the configured
// branch-prefix ownership rule.
type branchPolicyPRSelectSource struct {
	provider adoSelectProvider
}

func (s branchPolicyPRSelectSource) pullRequests(ctx context.Context, repo providers.RepositoryRef, request prSelectSourceRequest) ([]providers.PullRequestSummary, []providers.PullRequestSummary, error) {
	return pullRequestsForSelectionADO(
		ctx, s.provider, repo, request.base, request.headPrefixes, request.authorScope,
		request.identityFilters, request.triggerRef, request.completeness, request.expectedAuthorLogin,
	)
}

func (branchPolicyPRSelectSource) expectedAuthorLogin(context.Context, string) string { return "" }

type prSelectSafetyGateState struct {
	siblingBlocked    map[int]bool
	blockedDependents map[int]int
}

// loadPRSelectSafetyGateState performs the issue-comment-forge gates before
// eligibility. An absent provider is the explicit ADO limitation: no sibling
// is parked or aging-boosted, and no unsupported operation is attempted.
func loadPRSelectSafetyGateState(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	openPRs []providers.PullRequestSummary,
	base string,
	headPrefixes []string,
	expectedAuthorLogin string,
	stdout, stderr io.Writer,
) (prSelectSafetyGateState, int) {
	state := prSelectSafetyGateState{
		siblingBlocked:    make(map[int]bool),
		blockedDependents: make(map[int]int),
	}
	if provider == nil {
		return state, 0
	}

	blockerScanCtx, cancelBlockerScan := blockedOnSiblingScanContext(ctx)
	defer cancelBlockerScan()
	liveSiblingBlockers := make(map[int][]int)
	for _, pr := range openPRs {
		blockers, err := liveBlockedOnSiblingBlockers(blockerScanCtx, provider, repo, pr)
		if err != nil {
			return state, failProviderStage(stderr, fmt.Sprintf("check blocked-on-sibling state for PR #%d", pr.Number), err, "selected-pr.json")
		}
		liveSiblingBlockers[pr.Number] = blockers
		state.siblingBlocked[pr.Number] = len(blockers) > 0
		for _, blocker := range blockers {
			state.blockedDependents[blocker]++
		}
	}
	var couplingDependents []providers.PullRequestSummary
	for _, pr := range openPRs {
		if pr.State == "open" && pr.Base == base && isOwnPullRequest(pr.Author, pr.Head, headPrefixes, expectedAuthorLogin) &&
			!hasAnyLabel(pr.Labels, []string{noMergeReviewLabel}) {
			couplingDependents = append(couplingDependents, pr)
		}
	}
	couplings, couplingWarnings, err := loadFoundationCouplings(blockerScanCtx, provider, repo, couplingDependents, openPRs, state.siblingBlocked)
	if err != nil {
		return state, failProviderStage(stderr, "detect foundation-coupled pull requests", err, "selected-pr.json")
	}
	for _, warning := range couplingWarnings {
		pf(stderr, "warning: foundation-coupling scan: %s\n", warning)
	}
	for _, coupling := range couplings {
		changed, err := flagFoundationCoupling(
			blockerScanCtx, provider, repo, coupling, liveSiblingBlockers[coupling.dependent.Number],
		)
		if err != nil {
			return state, failProviderStage(stderr, fmt.Sprintf("flag foundation-coupled PR #%d", coupling.dependent.Number), err, "selected-pr.json")
		}
		if !changed {
			continue
		}
		liveSiblingBlockers[coupling.dependent.Number] = append(
			liveSiblingBlockers[coupling.dependent.Number], coupling.foundation.Number,
		)
		state.siblingBlocked[coupling.dependent.Number] = true
		state.blockedDependents[coupling.foundation.Number]++
		pf(stdout, "foundation-coupled: parked PR #%d behind PR #%d (%s)\n",
			coupling.dependent.Number, coupling.foundation.Number, strings.Join(coupling.files, ", "))
	}
	return state, 0
}

// prSelectSafetyGatesBlock applies the remaining issue-comment-forge gates to
// one otherwise eligible candidate. The nil ADO provider returns before any
// GitHub/Gitea-only helper can issue an unsupported operation.
func prSelectSafetyGatesBlock(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	pr providers.PullRequestSummary,
	siblingBlocked bool,
	stdout, stderr io.Writer,
) (bool, int) {
	if provider == nil {
		return false, 0
	}

	parked, err := scopeGateVerdictStillParks(ctx, provider, repo, pr)
	if err != nil {
		return false, failProviderStage(stderr, fmt.Sprintf("check scope-gate verdict for PR #%d", pr.Number), err, "selected-pr.json")
	}
	if parked {
		return true, 0
	}
	if isTutorBranch(pr.Head, providerBranchNamespace()) {
		classification, err := classifyRemoteTutorChanges(
			ctx, provider, repo, strconv.Itoa(pr.Number), pr.BaseSHA, pr.HeadSHA,
		)
		if err != nil {
			pf(stderr, "warning: could not classify Tutor PR #%d (%v) — requiring manual review\n", pr.Number, err)
			return true, 0
		}
		if classification.RequiresHumanSignoff() {
			pf(stdout, "manual review required for Tutor PR #%d: %s\n", pr.Number, classification.String())
			return true, 0
		}
	}
	blocked, err := escalationStillBlocks(ctx, provider, repo, pr)
	if err != nil {
		return false, failProviderStage(stderr, fmt.Sprintf("check escalation state for PR #%d", pr.Number), err, "selected-pr.json")
	}
	if blocked {
		return true, 0
	}
	// #950: a demoted PR (repeatedly could not merge at an unchanged head)
	// is excluded from selection so the election stops re-crowning the stuck
	// lander; its cluster drains around it via the blocked-on-sibling liveness
	// change. Fail OPEN so a demotion lookup error cannot create a merge outage.
	demoted, err := demotionStillHolds(ctx, provider, repo, pr)
	if err != nil {
		pf(stderr, "warning: could not resolve merge-demotion state for PR #%d (%v) — treating as not demoted\n", pr.Number, err)
		demoted = false
	}
	if demoted || siblingBlocked {
		return true, 0
	}
	return false, 0
}

func restrictSelectionToTargetedPullRequest(candidates []providers.PullRequestSummary, triggerRef string) []providers.PullRequestSummary {
	pullID, targeted := webhookhttp.PullNumberFromTriggerRef(triggerRef)
	if !targeted {
		return candidates
	}

	number, err := strconv.Atoi(pullID)
	if err != nil {
		return nil
	}

	for _, candidate := range candidates {
		if candidate.Number == number {
			return []providers.PullRequestSummary{candidate}
		}
	}
	return nil
}

func eligibleByMergeReviewPolicy(pr providers.PullRequestSummary, requiredOptInLabel string, respectAssignee bool, selfIdentity string) bool {
	if requiredOptInLabel != "" && !hasAnyLabel(pr.Labels, []string{requiredOptInLabel}) {
		return false
	}

	if respectAssignee && selfIdentity != "" &&
		!containsIdentity(pr.Assignees, selfIdentity) &&
		!containsIdentity(pr.RequestedReviewers, selfIdentity) {
		return false
	}
	return true
}

func mergeReviewPolicyRejection(pr providers.PullRequestSummary, requiredOptInLabel string, respectAssignee bool, selfIdentity string) string {
	if requiredOptInLabel != "" && !hasAnyLabel(pr.Labels, []string{requiredOptInLabel}) {
		return "missing required opt-in label " + requiredOptInLabel
	}
	if respectAssignee && selfIdentity != "" {
		return "not assigned to or requesting review from " + selfIdentity
	}
	return "policy rule mismatch"
}

func containsIdentity(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}

func mergeReviewEligibilityDescription(requiredOptInLabel string, respectAssignee bool, selfIdentity string) string {
	var rules []string
	if requiredOptInLabel != "" {
		rules = append(rules, "label:"+requiredOptInLabel)
	}
	if respectAssignee {
		rules = append(rules, "assignee-or-reviewer:"+selfIdentity)
	}
	if len(rules) == 0 {
		return "legacy"
	}
	return strings.Join(rules, ",")
}

// adoSelectProvider is the minimal mandatory-Provider surface pr-select's ADO
// branch reads through — satisfied by *providers.Dispatcher (production) and a
// fake (tests). Only mandatory Provider methods appear, so no optional ADO
// landing capability is probed during selection (merge-wiring-plan §2/§8).
type adoSelectProvider interface {
	ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
	PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error)
}

// pullRequestsForSelectionADO is pullRequestsForSelection's Azure DevOps
// counterpart (merge-wiring-plan §1b/§2). ADO has no RefCheckState/RefCheckStates
// and no GetPullRequest, so each candidate's CheckState — and its open/merged
// State, which ADO's ListPullRequests leaves empty — is resolved from
// PollPullRequest's branch-policy evaluations, and the webhook-targeted PR is
// resolved via PollPullRequest rather than GetPullRequest. The second return
// value preserves the shared source contract; the selection core receives it
// but skips the unsupported sibling/foundation scans for this source.
func pullRequestsForSelectionADO(
	ctx context.Context,
	provider adoSelectProvider,
	repo providers.RepositoryRef,
	base string,
	headPrefixes []string,
	authorScope string,
	identityFilters providers.ListPullRequestsRequest,
	triggerRef string,
	completeness prSelectSnapshotCompleteness,
	expectedAuthorLogin string,
) ([]providers.PullRequestSummary, []providers.PullRequestSummary, error) {
	openPRs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, SkipCheckState: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list open pull requests: %w", err)
	}
	pullID, targeted := webhookhttp.PullNumberFromTriggerRef(triggerRef)
	if targeted && completeness != prSelectCompleteSnapshot {
		pr, err := adoSelectionCandidate(ctx, provider, repo, pullID)
		if err != nil {
			return nil, nil, fmt.Errorf("read webhook pull request #%s: %w", pullID, err)
		}
		if !identityFilters.MatchesIdentityFields(pr.Author, pr.Assignees, pr.RequestedReviewers) {
			return nil, openPRs, nil
		}
		return []providers.PullRequestSummary{pr}, openPRs, nil
	}

	prs := make([]providers.PullRequestSummary, 0, len(openPRs))
	for _, pr := range openPRs {
		if authorScope != authorScopeAny && !isOwnPullRequest(pr.Author, pr.Head, headPrefixes, expectedAuthorLogin) {
			continue
		}
		if !identityFilters.MatchesIdentityFields(pr.Author, pr.Assignees, pr.RequestedReviewers) {
			continue
		}
		poll, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{
			Repository: repo, PullID: strconv.Itoa(pr.Number),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("read pull request #%d checks: %w", pr.Number, err)
		}
		pr.CheckState = poll.CheckState
		// ADO's ListPullRequests only returns active PRs but leaves Summary.State
		// empty; the eligibility filter gates on State=="open", so take the live
		// open/merged/abandoned mapping PollPullRequest already computed.
		if poll.State != "" {
			pr.State = poll.State
		}
		prs = append(prs, pr)
	}
	return prs, openPRs, nil
}

// adoSelectionCandidate resolves one Azure DevOps PR into the selection summary
// shape via PollPullRequest — ADO has no GetPullRequest (merge-wiring-plan §2).
// PollPullRequest already carries the branch-policy CheckState and the
// open/merged State, so the returned summary needs no RefCheckState follow-up.
func adoSelectionCandidate(ctx context.Context, provider adoSelectProvider, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error) {
	poll, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{
		Repository: repo, PullID: pullID,
	})
	if err != nil {
		return providers.PullRequestSummary{}, err
	}
	return providers.PullRequestSummary{
		ID:                 strconv.Itoa(poll.Number),
		Number:             poll.Number,
		URL:                poll.URL,
		Author:             poll.Author,
		Assignees:          poll.Assignees,
		RequestedReviewers: poll.RequestedReviewers,
		State:              poll.State,
		Merged:             poll.Merged,
		Head:               poll.HeadBranch,
		Base:               poll.BaseBranch,
		HeadSHA:            poll.HeadSHA,
		BaseSHA:            poll.BaseSHA,
		Draft:              poll.Draft,
		Labels:             poll.Labels,
		CheckState:         poll.CheckState,
		Body:               poll.Body,
	}, nil
}

func hasAnyHeadPrefix(head string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if prefix = strings.TrimSpace(prefix); prefix != "" && strings.HasPrefix(head, prefix) {
			return true
		}
	}
	return false
}

// isOwnPullRequest reports whether a PR (identified by its author login and
// head branch) is attributable to this daemon's own identity for
// merge-review's "is this ours" gate (#1780/#1295). When expectedAuthorLogin
// is set — a DaemonIdentity is configured and its login resolved — it
// compares author directly, the distinct-identity signal a real bot login
// gives for free, with zero reliance on branch naming. An empty
// expectedAuthorLogin falls back completely unchanged to the branch-prefix
// heuristic: today's only signal on a single-token instance, where every
// daemon-authored PR's author is indistinguishable from the operator's own
// account. Takes plain strings, not a providers.PullRequestSummary, so a
// caller that has only assembled the selected PR's fields piecemeal (see
// gather-sibling-context's consistency check) doesn't need to construct one.
func isOwnPullRequest(author, head string, headPrefixes []string, expectedAuthorLogin string) bool {
	if expectedAuthorLogin != "" {
		return strings.EqualFold(author, expectedAuthorLogin)
	}
	return hasAnyHeadPrefix(head, headPrefixes)
}

// daemonIdentityAuthorLogin resolves the login merge-review's "is this ours"
// check should compare pr.Author against, or "" to fall back to the
// branch-prefix heuristic unchanged (#1780). Loads instance.yaml directly
// (the same fallback path providerRepo already uses) rather than requiring a
// new runner-injected env var — root is already available to every
// provider-chain stage.
//
// PAT: github:pr:write already resolves to the DaemonIdentity's own token
// once configured (buildCredentials, runnerwiring.go), so provider — built
// from that exact token — reports the daemon identity's own login for free.
// GitHub App: an installation token cannot self-report a login (no
// equivalent of GET /user), so this requires Slug to be explicitly declared;
// unset (the default until #1779 lands) returns "" like no DaemonIdentity at
// all, not an error.
//
// A resolution failure (e.g. a transient network error on the live
// AuthenticatedLogin call) fails OPEN to the branch-prefix heuristic rather
// than failing the whole stage — a momentary identity-lookup hiccup must
// never block a merge-review cycle outright.
func daemonIdentityAuthorLogin(ctx context.Context, root string, provider remediationProvider) string {
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil || cfg.DaemonIdentity == nil {
		return ""
	}
	if cfg.DaemonIdentity.GitHubApp() {
		if cfg.DaemonIdentity.Slug == "" {
			return ""
		}
		return cfg.DaemonIdentity.Slug + "[bot]"
	}
	login, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return ""
	}
	return login
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

func hasPRSelectExclusion(labels, excludeLabels []string) bool {
	acknowledgedScopeGate := hasAnyLabel(labels, []string{scopeGateLabel}) &&
		hasAnyLabel(labels, []string{scopeGateAckLabel})
	for _, label := range excludeLabels {
		label = strings.TrimSpace(label)
		if acknowledgedScopeGate && label == needsRemediationLabel {
			continue
		}
		if hasAnyLabel(labels, []string{label}) {
			return true
		}
	}
	return false
}

// scopeGateVerdictStillParks skips only the exact PR state that was already
// reviewed as parked. A head/base change or operator acknowledgement changes
// the digest and makes the PR eligible for another review.
func scopeGateVerdictStillParks(
	ctx context.Context,
	provider authenticatedBacklogProvider,
	repo providers.RepositoryRef,
	pr providers.PullRequestSummary,
) (bool, error) {
	if !hasAnyLabel(pr.Labels, []string{scopeGateLabel}) ||
		hasAnyLabel(pr.Labels, []string{scopeGateAckLabel}) {
		return false, nil
	}
	digest := computeReviewDigest(pr.HeadSHA, pr.BaseSHA, pr.Labels)
	if digest == "" {
		return false, nil
	}
	author, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return false, fmt.Errorf("resolve merge-review verdict author: %w", err)
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return false, fmt.Errorf("list comments: %w", err)
	}
	for _, comment := range comments {
		if !isTrustedMergeReviewStatusComment(comment.Author, comment.Body, author) {
			continue
		}
		verdict, ok := parseVerdictComment(comment.Body)
		return ok &&
			strings.Contains(comment.Body, scopeGateParkedCommentMarker) &&
			cachedVerdictUsable(verdict, digest, pr.HeadSHA, pr.BaseSHA), nil
	}
	return false, nil
}
