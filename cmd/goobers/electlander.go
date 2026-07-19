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
func electionDecision(findings []apiv1.Finding, selectedNumber int, policy electionPolicyFunc) bool {
	if !allCrossPRBlocked(findings) {
		return false
	}
	return policy(selectedNumber, unionBlockingPRs(findings))
}

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
	fs.Usage = func() {
		pf(stderr, "Usage: goobers elect-lander [--gate name] [path]\n\n"+
			"Read the holistic review gate's Verdict from this run's journal and, when\n"+
			"it is entirely cross-PR-ordering asks and the selected PR is the elected\n"+
			"lander of its overlap cluster (lowest PR number), emit elected=true to\n"+
			"route the PR into merge-pr; otherwise emit elected=false to route it to\n"+
			"apply-verdict. Requires selectedNumber (inputsFrom gather-sibling-context).\n"+
			"Exit codes: 0 = decided (elected or not — both normal), 1 = business\n"+
			"error, 2 = usage/IO error.\n")
	}
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

	// #834: the lander-election policy is workflow-configurable. An unknown
	// name falls back to the deterministic default (fifo) rather than failing
	// the pipeline; log the fallback so a config typo is visible, not silent.
	policyName := providerInput("electionPolicy", defaultElectionPolicy)
	policy, resolvedPolicy := resolveElectionPolicy(policyName)
	if resolvedPolicy != policyName {
		pf(stderr, "warning: unknown election policy %q — falling back to %q\n", policyName, resolvedPolicy)
	}

	// writeResult emits the routing decision plus the pass-through outputs the
	// two possible successor stages resolve their inputsFrom against.
	writeResult := func(elected bool) int {
		data, err := json.Marshal(map[string]string{
			"elected":         strconv.FormatBool(elected),
			"selectedNumber":  strconv.Itoa(selectedNumber),
			"selectedHeadSha": selectedHeadSha,
			"selectedBaseSha": selectedBaseSha,
			"reviewDigest":    reviewDigest,
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
	verdict, err := readLatestGateVerdict(l.RunsDir(), runID, *gateName)
	if err != nil {
		pf(stderr, "error: read %s verdict from journal: %v\n", *gateName, err)
		return 1
	}
	if verdict == nil {
		pf(stderr, "error: no %s gate.evaluated event with a verdict found in this run's journal\n", *gateName)
		return 1
	}

	if !electionDecision(verdict.Findings, selectedNumber, policy) {
		pf(stdout, "PR #%d: not the elected lander under policy %q (or verdict carries a real defect) — routing to apply-verdict\n", selectedNumber, resolvedPolicy)
		return writeResult(false)
	}

	// This PR is the elected lander. Re-check the verdict's SHA pin against the
	// PR's current head/base (mirroring apply-verdict's D6 void check) so a
	// verdict computed against a state the PR has since moved past is not
	// crowned and merged — merge-pr re-verifies independently, but not electing
	// a stale verdict keeps the routing honest.
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
	headPrefix := providerInput("headPrefix", "goobers/")
	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, resultFile)
	}
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
		selectedNumber, unionBlockingPRs(verdict.Findings), resolvedPolicy)
	return writeResult(true)
}
