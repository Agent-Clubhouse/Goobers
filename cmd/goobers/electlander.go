package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// electedLander reports whether thisPR is the deterministically-elected lander
// of the mutually-sibling-blocked cluster it forms with the PRs it is blocked
// on. The V0.5 policy (#833) is lowest-PR-number: thisPR is the lander iff its
// number is lower than every PR it is blocked on.
//
// The deterministic overlap set makes every member of a clique independently
// compute the same global-minimum winner — no central coordination — so exactly
// one member is crowned and the rest park blocked-on-sibling. An empty blocker
// set (a cross-pr-blocked finding that named no sibling) trivially elects thisPR:
// there is no identified PR to defer to. #834 makes this policy pluggable.
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

// electedRace is the "race" election policy (#2268): thisPR is unconditionally
// the lander of its cluster, regardless of sibling PR numbers or their own
// state. Intended for a dedicated fast-track lane (e.g. a critical/urgent
// workflow instance) where coupling a defect-free PR's landing speed to
// sibling sequencing — even a "wins ties" version of that coupling — defeats
// the lane's entire purpose. This does not weaken #1071's single-lander
// safety invariant: electionDecision still requires electableUnderOrdering
// (no real defect on thisPR's own review) and an undemoted PR before it ever
// calls a policy, so "race" only ever fast-tracks a PR that is individually
// clean — it never lands a PR carrying its own genuine defect, and it never
// lets two overlap-cluster members land unarbitrated (thisPR alone is always
// elected, so callers still get exactly one winner per cluster). It simply
// declines to make that individually-clean PR wait for anyone else's turn.
func electedRace(thisPR int, blockers []int) bool {
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
	"race":   electedRace,
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
// cross-PR-ordering asks (electableUnderOrdering — the PR is individually fine
// and merely waiting on a sibling) AND this PR wins its cluster's election under
// the configured policy. Any verdict carrying a real defect (a substantive/
// conflict/rebase-needed finding above `info` severity) is never electable; an
// `info` finding is a nit and does not withhold landing authority (#1726).
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
	if !electableUnderOrdering(findings) {
		return false
	}
	return policy(selectedNumber, withoutDemoted(unionBlockingPRs(findings), demoted))
}

// electionClusterBlockers combines reviewer-named blockers with the
// deterministic overlap set. The latter remains available when a non-ordering
// finding makes withOverlapBackstop leave the reviewer's findings unchanged.
func electionClusterBlockers(findings []apiv1.Finding, overlappingSiblings []int) []int {
	combined := append([]apiv1.Finding(nil), findings...)
	combined = append(combined, apiv1.Finding{BlockingPRs: overlappingSiblings})
	return unionBlockingPRs(combined)
}

const noLanderEscalationPrefix = "Cluster has no lander under policy"

// noLanderEscalationReason detects the asymmetric zero-winner case: the
// deterministic policy winner cannot be crowned because its own review contains
// a real defect (severity above `info` — see findingIsRealDefect), while every
// otherwise-green sibling will defer to that winner.
// Escalating is safer than laundering the defect into a pass and more explicit
// than silently parking the rest of the cluster.
func noLanderEscalationReason(decision apiv1.VerdictDecision, findings []apiv1.Finding, selectedNumber int, overlappingSiblings []int, policy electionPolicyFunc, demoted map[int]bool, policyName string) string {
	if decision != apiv1.VerdictNeedsChanges || len(overlappingSiblings) == 0 ||
		demoted[selectedNumber] || electableUnderOrdering(findings) {
		return ""
	}
	clusterBlockers := electionClusterBlockers(findings, overlappingSiblings)
	if !policy(selectedNumber, withoutDemoted(clusterBlockers, demoted)) {
		return ""
	}
	siblings := make([]string, 0, len(clusterBlockers))
	for _, blocker := range clusterBlockers {
		siblings = append(siblings, fmt.Sprintf("#%d", blocker))
	}
	return fmt.Sprintf(
		noLanderEscalationPrefix+" %q: PR #%d is the deterministic winner over cluster sibling PRs %s, but its review contains non-ordering findings and it cannot be safely crowned; every other eligible sibling defers to that winner. Human intervention is required to resolve the winner's findings or choose a different landing order.",
		policyName, selectedNumber, strings.Join(siblings, ", "))
}

const electLanderHelp = "Usage: goobers elect-lander [--gate name] [path]\n\n" +
	"Read the holistic review gate's Verdict from this run's journal and, when\n" +
	"it is entirely cross-PR-ordering asks and the selected PR is the elected\n" +
	"lander of its overlap cluster (lowest PR number), emit elected=true to\n" +
	"route the PR into merge-pr; otherwise emit elected=false to route it to\n" +
	"apply-verdict. Advisory-mode PRs are never elected. Requires selectedNumber\n" +
	"and advisoryMode (inputsFrom gather-sibling-context).\n" +
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
// selectedNumber/selectedHeadSha/selectedBaseSha/reviewDigest and the scope-gate
// decision are threaded through as outputs so apply-verdict resolves its
// single-hop inputsFrom on this branch.
func runElectLander(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("elect-lander", flag.ContinueOnError)
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
	advisoryMode, err := strconv.ParseBool(providerInput("advisoryMode", "false"))
	if err != nil {
		pf(stderr, "error: invalid advisoryMode input: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "election.json")
	// Deterministic file-overlap set threaded from gather-sibling-context
	// (#990). Parsed for the election backstop; passed through verbatim so
	// apply-verdict on the parked (not-elected) branch resolves it too.
	overlappingSiblingsCsv := providerInput("overlappingSiblings", "")
	overlappingSiblings := parseOverlappingSiblings(overlappingSiblingsCsv)
	scopeGateParked := providerInput("scopeGateParked", "")

	// #834/#1028/#1029: the lander-election policy is workflow-configurable.
	// fifo/newest are pure functions; most-blockers/fewest-overlaps score every
	// cluster member from live cross-PR data and are resolved below, once the
	// open-PR set is in hand. An unknown name falls back to fifo.
	policyName := providerInput("electionPolicy", defaultElectionPolicy)

	// #2741: the sibling-serialization strategy — which cluster this stage
	// serializes against — is selectable independently of the ordering policy.
	// MUST match apply-verdict's siblingSerialization so both stages resolve
	// the same cluster.
	serializationInput := providerInput("siblingSerialization", defaultSiblingSerialization)
	serialization, knownSerialization := resolveSiblingSerialization(serializationInput)
	if !knownSerialization {
		pf(stderr, "warning: unknown sibling-serialization strategy %q — falling back to %q\n", serializationInput, serialization)
	}

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
			"advisoryMode":           strconv.FormatBool(advisoryMode),
			"scopeGateParked":        scopeGateParked,
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
	if advisoryMode {
		pf(stdout, "PR #%d is advisory-only — skipping lander election\n", selectedNumber)
		return writeResult(false)
	}

	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	verdict, err := readLatestGateVerdict(root, runID, *gateName)
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
	serializedCluster := serializationCluster(serialization, effectiveFindings, overlappingSiblings, selectedNumber)

	// The election needs the live open-PR set for two things: the elected
	// verdict's SHA-pin re-check below, and (#950) knowing which cluster members
	// are demoted so a stuck lander is dropped from candidacy and from every
	// sibling's blocker set. Set the provider up and list PRs up front so one
	// list feeds both, and so apply-verdict (which re-derives the same election)
	// resolves an identical demoted set from the same source.
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	stageCapability := capability.GitHubPRWrite
	if repo.Provider == providers.ProviderADO {
		stageCapability = capability.ADOPRWrite
	}
	provider, err := newMergeReviewRemediationProvider(root, repo,
		withStageProviderCapability(stageCapability),
		withStageProviderCache(),
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	base := providerInput("base", providerBaseBranch())
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
	clusterBlockers := electionClusterBlockers(effectiveFindings, overlappingSiblings)
	policy, resolvedPolicy, perr := resolveElectionPolicyForCluster(
		ctx, provider, repo, policyName, selectedNumber, clusterBlockers, prs)
	if perr != nil {
		if providers.IsNotFoundError(perr) {
			// A cluster-data policy (#1028/#1029) scores every member named as a
			// blocker; one of them has closed/merged since being recorded — the
			// same "no longer open" business outcome the selected PR itself gets
			// below, just for a cluster member instead. Never crown against stale
			// cluster membership: park explicitly (routing to apply-verdict with
			// the full pass-through envelope) rather than failing the stage.
			pf(stdout, "election policy %q could not score cluster member(s) — a named PR is no longer found (closed/merged) — election moot this cycle, routing to apply-verdict\n", policyName)
			return writeResult(false)
		}
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
	// The FIFO lander election (#950) is a GitHub merge-queue concept with no
	// Gitea equivalent; skip it on other forges rather than fail closed.
	if githubProvider, githubSelected := provider.(*providers.GitHubProvider); githubSelected {
		ineligible, ierr := electionIneligibleSet(ctx, githubProvider, repo, prs)
		if ierr != nil {
			return failProviderStage(stderr, "resolve lander eligibility", ierr, resultFile)
		}
		demoted = unionPRSets(demoted, ineligible)
	}

	if reason := noLanderEscalationReason(verdict.Decision, effectiveFindings, selectedNumber, serializedCluster, policy, demoted, resolvedPolicy); reason != "" {
		pf(stdout, "%s — routing to apply-verdict for explicit escalation\n", reason)
		return writeResult(false)
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
