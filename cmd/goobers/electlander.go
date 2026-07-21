package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// electedLander reports whether thisPR is the deterministically-elected lander
// of the mutually-sibling-blocked cluster it forms with the PRs it is blocked
// on. The V0.5 policy (#833) is lowest-PR-number: thisPR is the lander iff its
// number is lower than every PR it is blocked on.
//
// Under the symmetric findings the reviewer produces for file overlap (X
// overlaps Y iff Y overlaps X), every member of a clique independently computes
// the same global-minimum winner — no central coordination — so exactly one
// member is crowned and the rest park blocked-on-sibling. An empty blocker set
// (a cross-pr-blocked finding that named no sibling) trivially elects thisPR:
// there is no identified PR to defer to. #834 makes this policy pluggable; the
// merge queue's per-PR re-test backstops any asymmetric-finding edge case that
// briefly crowns two.
func electedLander(thisPR int, blockers []int) bool {
	for _, b := range blockers {
		if b < thisPR {
			return false
		}
	}
	return true
}

// electedNewest is the "newest" election policy (#834): highest PR number wins
// — thisPR is the lander iff its number is above every PR it is blocked on.
// Same exactly-one-winner guarantee as fifo under the reviewer's symmetric
// file-overlap findings, but elects the most-recently-opened cluster member
// (land the newest work first) rather than the oldest.
func electedNewest(thisPR int, blockers []int) bool {
	for _, b := range blockers {
		if b > thisPR {
			return false
		}
	}
	return true
}

// electionPolicyFunc decides whether thisPR is the elected lander given the PRs
// it is blocked on. Every registered policy is a pure function of
// {thisPR, blockers} so each cluster member computes the same winner
// independently — no central coordination (#834's seam over #833's fifo).
type electionPolicyFunc func(thisPR int, blockers []int) bool

// defaultElectionPolicy is the safe, boring, fully-reproducible default:
// lowest PR number (fifo).
const defaultElectionPolicy = "fifo"

// electionPolicies is the pluggable registry the elect-lander stage resolves
// its --policy / electionPolicy input against. Only purely-local deterministic
// policies live here today; cluster-data policies (most-blockers,
// fewest-overlaps) are tracked as follow-ups and would plug in the same way.
var electionPolicies = map[string]electionPolicyFunc{
	"fifo":   electedLander,
	"newest": electedNewest,
}

// resolveElectionPolicy returns the named policy and the name actually used. An
// unknown or empty name falls back to defaultElectionPolicy (fifo) rather than
// failing the whole merge-review pipeline on a config typo — the caller logs
// the fallback so a misconfigured policy is visible, not silent.
func resolveElectionPolicy(name string) (electionPolicyFunc, string) {
	if p, ok := electionPolicies[name]; ok {
		return p, name
	}
	return electedLander, defaultElectionPolicy
}

// electionDecision reports whether the selected PR should be crowned the lander
// of its cluster and routed to merge (#833). It is the pure core of the
// elect-lander stage: election fires only when the verdict is entirely
// cross-PR-ordering asks (allCrossPRBlocked — the PR is individually fine and
// merely waiting on a sibling) AND this PR wins its cluster's election under
// the configured policy. Any verdict carrying a real defect (a substantive/
// conflict/rebase-needed finding) is never electable — it routes to
// apply-verdict / pr-remediation unchanged.
func electionDecision(findings []apiv1.Finding, selectedNumber int, policy electionPolicyFunc, demoted map[int]bool) bool {
	// #950: a demoted lander (one that repeatedly could not merge at an
	// unchanged head) is never crowned — that is exactly the re-election that
	// deadlocks the cluster. And a demoted PR is dropped from the blocker set so
	// the next-lowest non-demoted member wins instead, draining the cluster
	// around the stuck one. demoted is empty in steady state (no PR carries
	// goobers:merge-demoted), so this is a no-op on the common path.
	if demoted[selectedNumber] {
		return false
	}
	if !allCrossPRBlocked(findings) {
		return false
	}
	return policy(selectedNumber, withoutDemoted(unionBlockingPRs(findings), demoted))
}

const electLanderHelp = "Usage: goobers elect-lander [--gate name] [path]\n\n" +
	"Read the holistic review gate's Verdict from this run's journal and, when\n" +
	"it is entirely cross-PR-ordering asks and the selected PR is the elected\n" +
	"lander of its overlap cluster (lowest PR number), emit elected=true to\n" +
	"route the PR into merge-pr; otherwise emit elected=false to route it to\n" +
	"apply-verdict. Requires selectedNumber (inputsFrom gather-sibling-context).\n" +
	"Exit codes: 0 = decided (elected or not — both normal), 1 = business\n" +
	"error, 2 = usage/IO error.\n"

// runElectLander implements `goobers elect-lander` (#833): merge-review's
// deterministic cross-PR winner-election stage, wired on the review gate's
// needs-changes branch. When the verdict is entirely cross-PR-ordering asks and
// this PR is the elected lander of its overlap cluster, it emits elected=true —
// routing the PR into merge-pr (via elect-gate's output-equals check) so
// exactly one member of a mutually-blocked cluster lands. Its merge then
// cascades the rest (post-merge fan-out + #748 unpark). Every other case emits
// elected=false, routing to apply-verdict unchanged (blocked-on-sibling for the
// non-elected members, needs-remediation for a verdict with real defects).
//
// selectedNumber/selectedHeadSha/selectedBaseSha/reviewDigest are threaded
// through as outputs so BOTH downstream stages resolve their single-hop
// inputsFrom on this branch: merge-pr on the elected path, apply-verdict on the
// parked path — the same pass-through post-merge/queue-watch already do for the
// merge-gate convergence.
func runElectLander(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("elect-lander", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gateName := fs.String("gate", "review", "the gate name whose verdict to read")
	fs.Usage = helpUsage(stderr, "elect-lander")
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

	selectedNumberStr := providerInput("selectedNumber", "")
	if selectedNumberStr == "" {
		pf(stderr, "error: selectedNumber is required (inputsFrom gather-sibling-context's selectedNumber output)\n")
		return 1
	}
	selectedNumber, err := strconv.Atoi(selectedNumberStr)
	if err != nil {
		pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
		return 1
	}
	selectedHeadSha := providerInput("selectedHeadSha", "")
	selectedBaseSha := providerInput("selectedBaseSha", "")
	reviewDigest := providerInput("reviewDigest", "")
	resultFile := providerInput("resultFile", "election.json")
	// Deterministic file-overlap set threaded from gather-sibling-context
	// (#990). Parsed for the election backstop; passed through verbatim so
	// apply-verdict on the parked (not-elected) branch resolves it too.
	overlappingSiblingsCsv := providerInput("overlappingSiblings", "")
	overlappingSiblings := parseOverlappingSiblings(overlappingSiblingsCsv)

	// #834/#1028/#1029: the lander-election policy is workflow-configurable.
	// fifo/newest are pure functions; most-blockers/fewest-overlaps score every
	// cluster member from live cross-PR data and are resolved below, once the
	// open-PR set is in hand. An unknown name falls back to fifo.
	policyName := providerInput("electionPolicy", defaultElectionPolicy)

	// writeResult emits the routing decision plus the pass-through outputs the
	// two possible successor stages resolve their inputsFrom against.
	writeResult := func(elected bool) int {
		data, err := json.Marshal(map[string]string{
			"elected":                strconv.FormatBool(elected),
			"selectedNumber":         strconv.Itoa(selectedNumber),
			"selectedHeadSha":        selectedHeadSha,
			"selectedBaseSha":        selectedBaseSha,
			"reviewDigest":           reviewDigest,
			"overlappingSiblingsCsv": overlappingSiblingsCsv,
		})
		if err != nil {
			pf(stderr, "error: marshal election result: %v\n", err)
			return 1
		}
		if err := os.WriteFile(resultFile, data, 0o644); err != nil {
			pf(stderr, "error: write %s: %v\n", resultFile, err)
			return 1
		}
		return 0
	}

	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	l := layoutFor(root)
	runsDir, err := runsDirForRun(l, runID)
	if err != nil {
		pf(stderr, "error: locate run journal: %v\n", err)
		return 1
	}
	verdict, err := readLatestGateVerdict(runsDir, runID, *gateName)
	if err != nil {
		pf(stderr, "error: read %s verdict from journal: %v\n", *gateName, err)
		return 1
	}
	if verdict == nil {
		pf(stderr, "error: no %s gate.evaluated event with a verdict found in this run's journal\n", *gateName)
		return 1
	}

	// Fold the deterministic overlap set (#990) into the findings used for the
	// election so a green PR whose only issue is a file collision is elected/
	// parked even if the reviewer under-named or missed the blocking siblings;
	// a verdict carrying a real defect is left unchanged (never electable).
	effectiveFindings := withOverlapBackstop(verdict.Findings, overlappingSiblings)

	// The election needs the live open-PR set for two things: the elected
	// verdict's SHA-pin re-check below, and (#950) knowing which cluster members
	// are demoted so a stuck lander is dropped from candidacy and from every
	// sibling's blocker set. Set the provider up and list PRs up front so one
	// list feeds both, and so apply-verdict (which re-derives the same election)
	// resolves an identical demoted set from the same source.
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token)
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", providerBranchNamespace())
	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, resultFile)
	}

	// Resolve the election policy now that the open-PR set is available — the
	// cluster-data policies (#1028/#1029) score every cluster member from it. A
	// gather failure fails the stage explicitly rather than silently electing a
	// different winner; apply-verdict resolves the same policy from the same
	// data, so the two stages agree on the crown.
	policy, resolvedPolicy, perr := resolveElectionPolicyForCluster(
		ctx, provider, repo, policyName, selectedNumber, unionBlockingPRs(effectiveFindings), prs)
	if perr != nil {
		return failProviderStage(stderr, "resolve election policy "+policyName, perr, resultFile)
	}
	if resolvedPolicy != policyName {
		pf(stderr, "warning: unknown election policy %q — falling back to %q\n", policyName, resolvedPolicy)
	}

	// #950: fail-safe — an unresolvable demotion state proceeds as an empty set
	// (exactly the pre-#950 behavior), and never blocks the election.
	demoted, derr := demotedSet(ctx, provider, repo, prs)
	if derr != nil {
		pf(stderr, "warning: could not resolve merge-demotion state (%v) — proceeding without it\n", derr)
		demoted = nil
	}

	if !electionDecision(effectiveFindings, selectedNumber, policy, demoted) {
		pf(stdout, "PR #%d: not the elected lander under policy %q (demoted, a real defect, or a lower non-demoted sibling wins) — routing to apply-verdict\n", selectedNumber, resolvedPolicy)
		return writeResult(false)
	}

	// This PR is the elected lander. Re-check the verdict's SHA pin against the
	// PR's current head/base (mirroring apply-verdict's D6 void check) so a
	// verdict computed against a state the PR has since moved past is not
	// crowned and merged — merge-pr re-verifies independently, but not electing
	// a stale verdict keeps the routing honest.
	var current *providers.PullRequestSummary
	for i := range prs {
		if prs[i].Number == selectedNumber {
			current = &prs[i]
			break
		}
	}
	if current == nil {
		pf(stdout, "PR #%d no longer open — election moot, routing to apply-verdict\n", selectedNumber)
		return writeResult(false)
	}
	if (verdict.HeadSHA != "" && verdict.HeadSHA != current.HeadSHA) ||
		(verdict.BaseSHA != "" && verdict.BaseSHA != current.BaseSHA) {
		pf(stdout, "PR #%d moved since review — election void, routing to apply-verdict\n", selectedNumber)
		return writeResult(false)
	}

	pf(stdout, "elected PR #%d as the lander of its blocked cluster (blockers %v, policy %q) — routing to merge\n",
		selectedNumber, unionBlockingPRs(effectiveFindings), resolvedPolicy)
	return writeResult(true)
}
