package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// blockedOnSiblingLabel marks a PR that's correct in isolation but must wait
// behind a named sibling (#747) — see verdictLabel's doc comment.
const blockedOnSiblingLabel = "goobers:blocked-on-sibling"

const mergeReviewStatusMarker = "<!-- goobers:merge-review-status -->"

const findingSetHistoryLimit = 10

type findingSetHistory struct {
	Hashes []string `json:"hashes"`
}

type canonicalFinding struct {
	Severity    apiv1.Severity     `json:"severity"`
	Class       apiv1.FindingClass `json:"class,omitempty"`
	Message     string             `json:"message"`
	Location    string             `json:"location,omitempty"`
	BlockingPRs []int              `json:"blockingPrs,omitempty"`
}

// verdictLabel maps a #358 Verdict's Decision to the design doc's label
// contract (§3): pass -> eligible to merge, needs-changes -> selected by
// pr-remediation, fail -> a human must look (§4 D2: fail is never burned on
// remediation budget, unlike needs-changes).
//
// needs-changes gets one further split (#747): when every finding is a pure
// cross-PR-ordering ask (FindingCrossPRBlocked) and there's at least one,
// the PR isn't broken — it's waiting on a sibling. Routing that to
// needs-remediation hands pr-remediation a defect that doesn't exist; it
// reproduces the identical diff, checkpoints byte-identical, and escalates
// (the stuck-loop pattern this issue exists to break). A mixed verdict —
// any substantive/conflict/rebase-needed finding present alongside
// cross-pr-blocked ones — still routes to needs-remediation unconditionally:
// a real defect takes priority regardless of ordering, and remediation can
// and should fix it.
func verdictLabel(decision apiv1.VerdictDecision, findings []apiv1.Finding) string {
	switch decision {
	case apiv1.VerdictPass:
		return "goobers:merge-ready"
	case apiv1.VerdictFail:
		return "goobers:merge-escalated"
	default:
		if allCrossPRBlocked(findings) {
			return blockedOnSiblingLabel
		}
		return "goobers:needs-remediation"
	}
}

// allCrossPRBlocked reports whether findings is non-empty and every finding
// in it is FindingCrossPRBlocked — an empty findings slice is deliberately
// NOT all-blocked (an empty needs-changes verdict with no findings at all is
// not a cross-PR-ordering situation; it falls through to needs-remediation
// like today).
// findingIsRealDefect reports whether a finding is a genuine reason to withhold
// landing authority, as opposed to an ordering note or a nit.
//
// Two things make a finding harmless for sequencing: it is a cross-pr-blocked
// ordering finding, or it is severity `info`. Everything else — including an
// unset or unrecognised severity — counts as a real defect, so this fails
// closed: a malformed verdict can never launder itself into landing authority.
//
// Severity used to be ignored entirely here, which deadlocked whole clusters
// (#1726). The trigger is mundane: two PRs that both run `make generate`
// produce a byte-identical patch, so they cluster, so the winner picks up
// `info` findings that say in their own text "there is no semantic conflict" —
// and those findings then withheld the crown. Live on 2026-07-26 that stalled
// PRs #1717/#1719/#1723/#1724 for over two hours on a verdict whose own summary
// called the winner "merge-ready". It recurs for every committed generated
// artifact whenever more than one PR is open.
//
// This mirrors the severity floor pr-remediation already applies via
// resolveMinSeverity/verdictHasSubstantiveFindingForPR (#941/PRR-6), including
// its treatment of an unset severity as significant.
func findingIsRealDefect(finding apiv1.Finding) bool {
	if finding.Class == apiv1.FindingCrossPRBlocked {
		return false
	}
	return finding.Severity != apiv1.SeverityInfo
}

// electableUnderOrdering reports whether findings leave the selected PR safely
// crownable: it must be sequencing-blocked (at least one ordering finding, which
// is what makes it a cluster member at all) and carry no real defect.
//
// Deliberately separate from allCrossPRBlocked rather than a change to it:
// allCrossPRBlocked also drives verdictLabel's blocked-on-sibling vs
// needs-remediation choice for every needs-changes PR, clustered or not, and
// widening that would change labelling far outside the election.
func electableUnderOrdering(findings []apiv1.Finding) bool {
	ordering := false
	for _, finding := range findings {
		if findingIsRealDefect(finding) {
			return false
		}
		if finding.Class == apiv1.FindingCrossPRBlocked {
			ordering = true
		}
	}
	return ordering
}

func allCrossPRBlocked(findings []apiv1.Finding) bool {
	if len(findings) == 0 {
		return false
	}
	for _, f := range findings {
		if f.Class != apiv1.FindingCrossPRBlocked {
			return false
		}
	}
	return true
}

// unionBlockingPRs collects the deduplicated, sorted union of BlockingPRs
// across every finding — a verdict can carry more than one cross-pr-blocked
// finding (e.g. two independent ordering asks against two different
// siblings), and blockedOnSiblingState.Blockers records the full set, not
// just the first finding's.
func unionBlockingPRs(findings []apiv1.Finding) []int {
	seen := make(map[int]bool)
	var out []int
	for _, f := range findings {
		for _, pr := range f.BlockingPRs {
			if !seen[pr] {
				seen[pr] = true
				out = append(out, pr)
			}
		}
	}
	sort.Ints(out)
	return out
}

// parseOverlappingSiblings parses the comma-separated overlappingSiblings
// input — the deterministic file-overlap set gather-sibling-context computes
// (#989/#990) and threads through the workflow — into PR numbers, skipping
// blank or unparseable tokens.
func parseOverlappingSiblings(csv string) []int {
	var out []int
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if n, err := strconv.Atoi(tok); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// withOverlapBackstop folds the deterministic file-overlap set (#990) into a
// verdict's findings so sequencing routing uses ground truth, not only the LLM
// reviewer's classification. Conservative and additive:
//
//   - A substantive finding whose location names only siblings in the
//     deterministic overlap set is normalized to cross-pr-blocked. This is the
//     #2478 shape: pure overlap was misclassified as a selected-PR defect.
//   - If any real defect remains (see findingIsRealDefect), the normalized
//     findings are returned without adding the overlap backstop. A real bug,
//     conflict, or rebase need takes priority and must route to remediation.
//   - Otherwise, a cross-pr-blocked finding carrying any still-unnamed
//     overlapping siblings is appended, so allCrossPRBlocked /
//     unionBlockingPRs / electionDecision treat the PR as sequencing-blocked on
//     the whole deterministic cluster even if the reviewer under-named the
//     blocking PRs or filed no structured finding at all.
//
// The returned slice never aliases the caller's backing array (full-slice
// append), so the published verdict's own findings stay the reviewer's.
func withOverlapBackstop(findings []apiv1.Finding, overlappingSiblings []int) []apiv1.Finding {
	if len(overlappingSiblings) == 0 {
		return findings
	}
	normalized := append([]apiv1.Finding(nil), findings...)
	for i, f := range normalized {
		if blockers, ok := overlapOnlyBlockingPRs(f, overlappingSiblings); ok {
			normalized[i].Class = apiv1.FindingCrossPRBlocked
			normalized[i].BlockingPRs = blockers
		}
	}
	for _, f := range normalized {
		if findingIsRealDefect(f) {
			return normalized
		}
	}
	named := make(map[int]bool)
	for _, blocker := range unionBlockingPRs(normalized) {
		named[blocker] = true
	}
	missing := make([]int, 0, len(overlappingSiblings))
	for _, sibling := range overlappingSiblings {
		if !named[sibling] {
			missing = append(missing, sibling)
			named[sibling] = true
		}
	}
	if len(missing) == 0 {
		return normalized
	}
	return append(normalized, apiv1.Finding{
		Severity:    apiv1.SeverityWarning,
		Class:       apiv1.FindingCrossPRBlocked,
		Message:     fmt.Sprintf("deterministic file overlap with sibling PR(s) %v — sequencing required", missing),
		BlockingPRs: missing,
	})
}

func overlapOnlyBlockingPRs(finding apiv1.Finding, overlappingSiblings []int) ([]int, bool) {
	if finding.Class != apiv1.FindingSubstantive {
		return nil, false
	}
	overlapping := make(map[int]bool, len(overlappingSiblings))
	for _, number := range overlappingSiblings {
		overlapping[number] = true
	}
	refs := prReferencePattern.FindAllStringSubmatch(finding.Location, -1)
	if len(refs) == 0 {
		return nil, false
	}
	seen := make(map[int]bool, len(refs))
	blockers := make([]int, 0, len(refs))
	for _, ref := range refs {
		if len(ref) < 2 {
			return nil, false
		}
		number, err := strconv.Atoi(ref[1])
		if err != nil || !overlapping[number] {
			return nil, false
		}
		if !seen[number] {
			seen[number] = true
			blockers = append(blockers, number)
		}
	}
	sort.Ints(blockers)
	return blockers, true
}

// predecessorBlockers narrows a parked PR's recorded blockers to only the
// cluster members that must land BEFORE it under the election order (#991) —
// its predecessors — rather than the symmetric union of every overlapping
// sibling. This is what lets a cluster of 3+ drain instead of deadlocking:
// with the symmetric set, member B lists C and C lists B, so neither ever
// unparks (unparkResolvedSiblings needs ALL named blockers closed). With
// predecessors only, each member waits solely on those ordered ahead of it,
// so the cluster drains one landing at a time.
//
// A blocker b is a predecessor of thisPR iff, in the two-member sub-cluster
// {thisPR, b}, thisPR is NOT the elected lander — i.e. b lands first. Reusing
// the election policy this way keeps predecessor-order and election-order
// identical for every policy (fifo: lower first; newest: higher first)
// without re-encoding the ordering per policy.
func predecessorBlockers(thisPR int, blockers []int, policy electionPolicyFunc, demoted map[int]bool) []int {
	var out []int
	// #950: a demoted predecessor no longer blocks its successors — dropping it
	// here is what lets a parked sibling unpark and be re-selected while the
	// stuck lander is worked separately. Empty demoted set in steady state, so
	// this is identical to the pre-#950 behavior on the common path.
	for _, b := range withoutDemoted(blockers, demoted) {
		if b == thisPR {
			continue
		}
		if !policy(thisPR, []int{b}) {
			out = append(out, b)
		}
	}
	sort.Ints(out)
	return out
}

// blockedOnSiblingState is the PR-altitude analog of blockedrecords.go's
// backlog-altitude blockedRecord (#747) — the structured record apply-verdict
// posts when a verdict's findings are entirely cross-PR-ordering asks. This
// is the source of truth #748's selection-exclusion/self-heal reads: which
// PR(s) this one is genuinely waiting behind, so it can be excluded from
// re-selection until they close and unparked once they do — without that
// consulting a full Verdict's Findings array.
type blockedOnSiblingState struct {
	// Blockers is the union of BlockingPRs across every cross-pr-blocked
	// finding in the verdict that produced this record.
	Blockers []int `json:"blockers"`
	// Reason is the verdict's own rationale, for a human reading the comment.
	Reason string `json:"reason"`
	// HeadSHA/BaseSHA pin the PR state this record was computed against —
	// same SHA-pinning discipline as Verdict's own HeadSHA/BaseSHA (design
	// doc §6 D6).
	HeadSHA string `json:"headSha"`
	BaseSHA string `json:"baseSha"`
	// RecordedAt is when this record was posted.
	RecordedAt time.Time `json:"recordedAt"`
}

// blockedOnSiblingPattern matches the machine-readable payload
// blockedOnSiblingComment appends — mirrors verdictJSONPattern above.
var blockedOnSiblingPattern = regexp.MustCompile(`(?s)<!-- blocked-on-sibling: (.*?) -->`)

// blockedOnSiblingComment marshals s into the HTML-comment payload appended
// to the posted verdict comment — mirrors verdictJSONComment above, and
// #716's remediationState/remediationStateComment pattern
// (cmd/goobers/remediationcheckpoint.go): always a fresh append onto the
// SAME comment apply-verdict is already posting (renderVerdictComment's own
// doc comment explains why: one posted comment stays the single source of
// truth, rather than growing a second, driftable channel), never an
// in-place edit of a prior comment.
func blockedOnSiblingComment(s blockedOnSiblingState) (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal blocked-on-sibling payload: %w", err)
	}
	return fmt.Sprintf("<!-- blocked-on-sibling: %s -->", data), nil
}

// parseBlockedOnSiblingComment recovers the blockedOnSiblingState a
// prior apply-verdict run embedded in a PR comment — the read side #748's
// selection-exclusion/self-heal uses. Returns ok=false if body has no
// embedded payload, the normal case for any comment apply-verdict didn't
// post as blocked-on-sibling.
func parseBlockedOnSiblingComment(body string) (s blockedOnSiblingState, ok bool) {
	m := blockedOnSiblingPattern.FindStringSubmatch(body)
	if m == nil {
		return blockedOnSiblingState{}, false
	}
	if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
		return blockedOnSiblingState{}, false
	}
	return s, true
}

func verdictPinVoidReason(verdict apiv1.Verdict, selectedHeadSHA, selectedBaseSHA, currentHeadSHA, currentBaseSHA string) string {
	if verdict.HeadSHA != "" && verdict.HeadSHA != selectedHeadSHA {
		return fmt.Sprintf("reviewer echoed head SHA %q, but deterministic review pin is %q", verdict.HeadSHA, selectedHeadSHA)
	}
	if verdict.BaseSHA != "" && verdict.BaseSHA != selectedBaseSHA {
		return fmt.Sprintf("reviewer echoed base SHA %q, but deterministic review pin is %q", verdict.BaseSHA, selectedBaseSHA)
	}
	if selectedHeadSHA != currentHeadSHA {
		return fmt.Sprintf("PR head moved from deterministic review pin %q to %q", selectedHeadSHA, currentHeadSHA)
	}
	if selectedBaseSHA != currentBaseSHA {
		return fmt.Sprintf("PR base moved from deterministic review pin %q to %q", selectedBaseSHA, currentBaseSHA)
	}
	return ""
}

const applyVerdictHelp = "Usage: goobers apply-verdict [--gate name] [path]\n\n" +
	"Read the holistic review gate's Verdict from this run's own journal,\n" +
	"cross-check its optional SHA echo against the deterministic review\n" +
	"pin, re-check that pin against the PR's current head/base, and — if\n" +
	"still valid — publish the verdict. Managed PRs receive a native GitHub\n" +
	"review plus the PR-comment handoff; non-pass verdicts also receive a\n" +
	"remediation label. advisoryMode=true publishes only the non-blocking\n" +
	"comment, without closing, labeling, electing, or merging the PR. A\n" +
	"stale SHA pin voids the verdict: no comment, no label, exit 0 (this\n" +
	"cycle's work is simply moot, not an error — merge-review re-reviews\n" +
	"next tick). Requires selectedNumber, selectedHeadSha, and\n" +
	"selectedBaseSha from Task.InputsFrom. Exit codes: 0 = applied (or\n" +
	"voided), 1 = business error, 2 = usage/IO error.\n"

// runApplyVerdict implements `goobers apply-verdict` (issue #359): reads the
// holistic review gate's Verdict back from this run's own journal (the gate
// already records it as an artifact via internal/gate's recordVerdict — no
// new plumbing), cross-checks its SHA echo against gather-sibling-context's
// authoritative pin, and re-checks that pin against the PR's CURRENT head/base
// before acting (design doc §6 D6: a verdict computed against a state that no
// longer exists is void, not actionable). Managed PRs receive a SHA-pinned
// native GitHub review plus the existing prose comment handoff consumed by
// merge, cache, and remediation paths; non-pass verdicts additionally retain
// their decision labels, except that an active merge escalation suppresses
// needs-remediation until the escalation is cleared or self-heals. Advisory
// PRs receive only the non-blocking comment.
//
// Before posting, a verdict missing Digest/SourceRunID (issue #523: every
// genuinely fresh, reviewer-produced verdict — a cache-hit verdict already
// carries both, reused unchanged from whichever run originally posted it)
// is stamped with reviewDigest (gather-sibling-context's own computed
// input, threaded via inputsFrom) and this run's GOOBERS_RUN_ID. This is
// what makes the verdict this comment posts findable and reusable by the
// NEXT gather-sibling-context's cache lookup — the digest travels with the
// verdict, not as separate state.
func runApplyVerdict(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("apply-verdict", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gateName := fs.String("gate", "review", "the gate name whose verdict to apply")
	fs.Usage = helpUsage(stderr, "apply-verdict")
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
	resultFile := providerInput("resultFile", "verdict-result.json")

	selectedNumberStr := providerInput("selectedNumber", "")
	if selectedNumberStr == "" {
		pf(stderr, "error: selectedNumber is required (inputsFrom pr-select's number output)\n")
		return 1
	}
	selectedNumber, err := strconv.Atoi(selectedNumberStr)
	if err != nil {
		pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
		return 1
	}
	selectedHeadSHA := providerInput("selectedHeadSha", "")
	if selectedHeadSHA == "" {
		pf(stderr, "error: selectedHeadSha is required (inputsFrom gather-sibling-context's deterministic output)\n")
		return 1
	}
	selectedBaseSHA := providerInput("selectedBaseSha", "")
	if selectedBaseSHA == "" {
		pf(stderr, "error: selectedBaseSha is required (inputsFrom gather-sibling-context's deterministic output)\n")
		return 1
	}
	advisoryMode, err := strconv.ParseBool(providerInput("advisoryMode", "false"))
	if err != nil {
		pf(stderr, "error: invalid advisoryMode input: %v\n", err)
		return 1
	}
	publishAdvisory, err := strconv.ParseBool(providerInput("publishAdvisory", "true"))
	if err != nil {
		pf(stderr, "error: invalid publishAdvisory input: %v\n", err)
		return 1
	}

	runID, workflowName, err := providerRunContext()
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
	if err := validateVerdictForPublish(*verdict); err != nil {
		pf(stderr, "error: validate %s verdict from journal: %v\n", *gateName, err)
		return 1
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider, err := newApplyVerdictProviderForRepo(root, repo)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Every publish-path helper below reaches only ListComments/UpdateComment/
	// AuthenticatedLogin/UpdateWorkItem/ListPullRequests/PullRequestFiles/
	// BranchTipSHA/GetPullRequest, all of which GiteaProvider implements and
	// all of which sit on remediationProvider. The old *GitHubProvider type
	// assertion below refused to publish ANY non-moot verdict on Gitea, so a
	// self-hosted instance could review a PR but never tell the PR about it:
	// the findings stayed in the run journal, the PR was never labelled
	// goobers:needs-remediation, and pr-remediation had nothing to select.
	prProvider, providerRouted := provider.(remediationProvider)
	if !providerRouted && repo.Provider != providers.ProviderADO {
		pf(stderr, "error: apply-verdict does not support repository provider %q\n", repo.Provider)
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	if advisoryMode {
		if !providerRouted {
			pf(stderr, "error: apply-verdict advisory mode is not supported for repository provider %q\n", repo.Provider)
			return 1
		}
		return applyAdvisoryVerdict(
			ctx, prProvider, repo, selectedNumber, selectedNumberStr, selectedHeadSHA, selectedBaseSHA,
			*verdict, runID, resultFile, publishAdvisory, stdout, stderr,
		)
	}

	base := providerInput("base", providerBaseBranch())
	headPrefix := providerInput("headPrefix", providerBranchNamespace())
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, "")
	}

	// #950: which open PRs are currently demoted (repeatedly could not merge at
	// an unchanged head). The election drops these from candidacy and from every
	// sibling's blocker set so a stuck FIFO-minimum lander cannot deadlock its
	// cluster. Fail-safe: on any resolution error, proceed with an empty set —
	// the demotion signal must never itself become a merge outage, and an empty
	// set is exactly the pre-#950 behavior. Reuses the prs list already fetched
	// above; only currently-labeled PRs cost an extra ListComments.
	var demoted map[int]bool
	if providerRouted {
		var derr error
		demoted, derr = demotedSet(ctx, prProvider, repo, prs)
		if derr != nil {
			pf(stderr, "warning: could not resolve merge-demotion state (%v) — proceeding without it\n", derr)
			demoted = nil
		}
		// The FIFO lander election (#950) is a GitHub merge-queue concept with
		// no Gitea equivalent; skip it on other forges rather than fail closed.
		if githubProvider, githubSelected := provider.(*providers.GitHubProvider); githubSelected {
			ineligible, ierr := electionIneligibleSet(ctx, githubProvider, repo, prs)
			if ierr != nil {
				return failProviderStage(stderr, "resolve lander eligibility", ierr, "")
			}
			demoted = unionPRSets(demoted, ineligible)
		}
	}

	current, err := currentPullRequest(ctx, provider, repo, selectedNumberStr)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("get pull request #%d", selectedNumber), err, "")
	}
	if current.State != "open" || current.Merged {
		pln(stdout, "PR is no longer open (merged/closed since selection) — verdict moot, nothing to apply")
		return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, "moot", "", stderr)
	}
	if hasAnyLabel(current.Labels, []string{noMergeReviewLabel}) {
		pln(stdout, "PR opted out of merge-review since selection — verdict moot, nothing to apply")
		return writeApplyVerdictResultWithReason(
			resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, "moot", "",
			"PR carries "+noMergeReviewLabel, stderr,
		)
	}

	// D6: gather-sibling-context's deterministic pin is authoritative. The
	// reviewer's optional echo can disprove that it reviewed the gathered diff,
	// but omitting the echo cannot bypass the current-state check.
	if reason := verdictPinVoidReason(*verdict, selectedHeadSHA, selectedBaseSHA, current.HeadSHA, current.BaseSHA); reason != "" {
		pf(stdout, "verdict void for PR #%d: %s — skipping, will re-review next cycle\n", selectedNumber, reason)
		// Voiding used to publish nothing at all, which left whatever verdict was
		// already on the PR standing as if it were current. pr-remediation reads
		// that comment, so it kept fixing findings from a superseded head, pushing
		// — which moved the head again — and voiding the next review in turn. PR
		// #1719 burned 3 hours over 4 cycles that way, each including a full local
		// `make ci`, against a review that could no longer see any of its own
		// fixes (#1733).
		//
		// Marking the standing verdict stale breaks that loop at its information
		// source: remediation can tell "no current review" from "these findings
		// still stand". Best-effort — a comment write must never turn a moot
		// verdict into a stage failure, since the re-review next cycle is what
		// actually resolves this.
		if providerRouted {
			if cerr := markMergeReviewVerdictStale(ctx, prProvider, repo, selectedNumber, reason); cerr != nil {
				pf(stderr, "warning: could not mark PR #%d's verdict stale: %v\n", selectedNumber, cerr)
			}
		}
		return writeApplyVerdictResultWithReason(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, "moot", "", reason, stderr)
	}

	// Close a pull request that is NO LONGER NEEDED rather than parking,
	// merging, or escalating it (#923/#947/#987). Every trigger is a
	// deterministic, independently verifiable repository fact, never the
	// reviewer's prose:
	//
	//   - Moot on ANY decision (broadened from the old fail-only path): its
	//     diff against base is now empty (already landed elsewhere), or every
	//     issue it exists to close is already closed (a stale issue closed
	//     mid-flight). See mootFailReason.
	//   - True duplicate, only for a NON-passing PR: an earlier open goober PR
	//     already implements the same issue. A passing PR is never closed as a
	//     duplicate — it merges and wins, and the redundant earlier PR then
	//     becomes moot (its issue closed by that merge) and closes on its own
	//     next review. See duplicateOfEarlierPR.
	// Never intercept a PASS: a passing PR merges and wins. For a non-passing
	// PR, close it if it is no longer needed.
	if verdict.Decision != apiv1.VerdictPass {
		if reason, moot := mootFailReason(ctx, provider, repo, &current); moot {
			return closeMootPullRequest(ctx, provider, repo, selectedNumber, &current, *verdict, reason, resultFile, stdout, stderr)
		}
		if repo.Provider == providers.ProviderADO {
			// Azure DevOps non-pass bridge (remediation-wiring plan Part 1): a
			// needs-changes/fail verdict that is NOT objectively moot is handed off
			// to the pr-remediation loop — a failed goobers/validation PR status, a
			// goobers:needs-remediation PR label (via the native PR-labels endpoint,
			// never UpdateWorkItem(ID: PR#), the wrong-object hazard), and the
			// findings + verdict-json on a PR thread — instead of the old hard-fail.
			// The GitHub-only duplicate/superseded-sibling closes below read PR
			// *issue* comments and submit a native review, neither of which exists
			// on ADO, so the ADO path returns here.
			if adoProvider, ok := provider.(*providers.ADOProvider); ok {
				return publishADONonPassVerdict(ctx, adoProvider, repo, selectedNumber, current, *verdict, resultFile, stdout, stderr)
			}
			pf(stderr, "error: apply-verdict can close an objectively moot %s pull request, but publishing a non-moot verdict is not supported for that provider\n", repo.Provider)
			return 1
		}
		if reason, dup := duplicateOfEarlierPR(ctx, prProvider, repo, &current); dup {
			return closeMootPullRequest(ctx, provider, repo, selectedNumber, &current, *verdict, reason, resultFile, stdout, stderr)
		}
		// Superseded by a byte-identical earlier open sibling (#1211): two PRs
		// that implement DIFFERENT issues can still converge to the identical
		// tree, which duplicateOfEarlierPR (shared-issue only) misses — the
		// deadlock #1179/#1180 filed. Same disposition: the earlier one wins,
		// this redundant later one is closed as no-longer-needed.
		if reason, superseded := supersededByIdenticalSibling(ctx, prProvider, repo, &current); superseded {
			return closeMootPullRequest(ctx, provider, repo, selectedNumber, &current, *verdict, reason, resultFile, stdout, stderr)
		}
	}
	if repo.Provider == providers.ProviderADO {
		// Azure DevOps: a PASS verdict is published as a provider-native PR
		// status (genre goobers, name validation — the same surface
		// report-pr-status publishes) so the published-verdict gate and any ADO
		// status-check branch policy observe it. The GitHub verdict-publication
		// path below (native self-review + sticky comment + PR-as-work-item
		// label write) does not apply on ADO: there is no self-review to submit,
		// and UpdateWorkItem(ID: PR#) would mutate the unrelated work item that
		// shares the PR's numeric id (the wrong-object hazard, ~789 below).
		// Non-pass ADO verdicts are handled by the earlier publishADONonPassVerdict
		// bridge (they return before reaching here); this gate is reached on ADO
		// only for a PASS.
		if adoProvider, ok := provider.(*providers.ADOProvider); ok && verdict.Decision == apiv1.VerdictPass {
			return publishADOPassVerdict(ctx, adoProvider, repo, selectedNumber, current, resultFile, stdout, stderr)
		}
		pf(stderr, "error: apply-verdict can close an objectively moot %s pull request, but publishing a non-moot verdict is not supported for that provider\n", repo.Provider)
		return 1
	}
	posted := *verdict
	posted.HeadSHA = selectedHeadSHA
	posted.BaseSHA = selectedBaseSHA
	if posted.Digest == "" {
		posted.Digest = providerInput("reviewDigest", "")
	}
	if posted.SourceRunID == "" {
		posted.SourceRunID = runID
	}

	// Fold the deterministic file-overlap set (#990/#2486) into the findings used
	// for sequencing and publication. Publishing the normalized representation
	// keeps downstream consumers from rediscovering the reviewer's classification
	// miss; a verdict with any real defect still routes to remediation.
	overlappingSiblings := parseOverlappingSiblings(providerInput("overlappingSiblings", ""))
	posted.OverlapCluster = len(overlappingSiblings) > 0
	effective := posted
	effective.Findings = withOverlapBackstop(posted.Findings, overlappingSiblings)
	posted.Findings = effective.Findings

	// Resolve the election policy once, gathering cluster data for the
	// cluster-data policies (#1028/#1029) from the same open-PR list and cluster
	// elect-lander used — so this stage's re-derived election matches
	// elect-lander's exactly (both the crown below and the predecessor order in
	// the parked-record). A gather failure fails the stage explicitly rather
	// than silently deriving a different winner.
	policyInput := providerInput("electionPolicy", defaultElectionPolicy)
	clusterBlockers := electionClusterBlockers(effective.Findings, overlappingSiblings)
	clusterPolicy, resolvedPolicyName, perr := resolveElectionPolicyForCluster(
		ctx, prProvider, repo, policyInput, selectedNumber, clusterBlockers, prs)
	if perr != nil {
		return failProviderStage(stderr, "resolve election policy "+policyInput, perr, "")
	}

	// A deterministic winner with a real defect cannot safely land, and every
	// sibling will defer to it. Publish that zero-winner state as a distinct
	// human escalation instead of silently splitting the cluster between
	// blocked-on-sibling and needs-remediation.
	if reason := noLanderEscalationReason(posted.Decision, effective.Findings, selectedNumber, overlappingSiblings, clusterPolicy, demoted, resolvedPolicyName); reason != "" {
		posted.Decision = apiv1.VerdictFail
		posted.Rationale = reason
	}

	// Election resolves single-lander status for EVERY sibling-overlap PR —
	// not only one a reviewer classified needs-changes — so a genuinely
	// clean review still waits its turn behind a live predecessor (#1071):
	// GitHub's native merge queue must never be a second, uncoordinated
	// merge authority that crowns a cluster member on its own. See
	// resolveElectionOutcome.
	if elected, rationale := resolveElectionOutcome(selectedNumber, posted.Decision, effective.Findings, posted.Rationale, overlappingSiblings, demoted, clusterPolicy, resolvedPolicyName); rationale != "" {
		posted.Elected = elected
		posted.Rationale = rationale
		if elected {
			posted.Decision = apiv1.VerdictPass
		} else {
			posted.Decision = apiv1.VerdictNeedsChanges
			posted.Findings = effective.Findings
		}
	}

	verdictAuthor, err := prProvider.AuthenticatedLogin(ctx)
	if err != nil {
		return failProviderStage(stderr, "resolve merge-review verdict author", err, resultFile)
	}
	statusComments, err := provider.ListComments(ctx, repo, selectedNumberStr)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("load finding-set history for PR #%d", selectedNumber), err, resultFile)
	}
	priorHistory, err := findingSetHistoryFromComments(statusComments, verdictAuthor)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("read finding-set history for PR #%d", selectedNumber), err, resultFile)
	}
	history, findingHash, revisited, err := advanceFindingSetHistory(priorHistory, posted.Findings)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("hash finding set for PR #%d", selectedNumber), err, resultFile)
	}
	oscillated := revisited &&
		posted.Decision == apiv1.VerdictNeedsChanges &&
		verdictLabel(posted.Decision, effective.Findings) == needsRemediationLabel
	if oscillated {
		reason := fmt.Sprintf(
			"Finding-set oscillation detected: `%s` matches an earlier merge-review state. Remediation returned to a prior unresolved finding set, so this PR is escalated instead of spending the remaining repass budget.",
			findingHash,
		)
		posted.Decision = apiv1.VerdictFail
		if posted.Rationale == "" {
			posted.Rationale = reason
		} else {
			posted.Rationale += "\n\n" + reason
		}
	}

	if err := validateVerdictForPublish(posted); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("validate verdict for PR #%d", selectedNumber), err, resultFile)
	}
	comment := renderScopeGateStateComment(
		renderVerdictComment(posted),
		providerInput("scopeGateParked", "") == "true",
	)
	historyPayload, err := findingSetHistoryComment(history)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("render finding-set history for PR #%d", selectedNumber), err, resultFile)
	}
	comment += "\n\n" + historyPayload
	label := verdictLabel(posted.Decision, effective.Findings)
	escalationSuppressedRemediation := false
	addLabels := []string{label}
	var removeLabels []string
	switch label {
	case needsRemediationLabel:
		escalationSuppressedRemediation, err = verdictEscalationStillBlocks(ctx, provider, repo, current)
		if err != nil {
			return failProviderStage(stderr, fmt.Sprintf("check active escalation for PR #%d", selectedNumber), err, resultFile)
		}
		if escalationSuppressedRemediation {
			addLabels = nil
			removeLabels = []string{needsRemediationLabel}
		}
	case remediationEscalatedLabel:
		removeLabels = []string{needsRemediationLabel}
	case blockedOnSiblingLabel:
		removeLabels = []string{needsRemediationLabel}
	}
	if label == blockedOnSiblingLabel {
		// Record only the predecessors this parked PR must wait behind, not the
		// symmetric union of every overlapping sibling (#991) — otherwise a 3+
		// cluster deadlocks (each member lists the others, none ever unparks).
		// Uses the same cluster-resolved policy as the crown above (#1028/#1029),
		// so predecessor-order and election-order stay identical.
		state := blockedOnSiblingState{
			Blockers:   predecessorBlockers(selectedNumber, unionBlockingPRs(effective.Findings), clusterPolicy, demoted),
			Reason:     posted.Rationale,
			HeadSHA:    posted.HeadSHA,
			BaseSHA:    posted.BaseSHA,
			RecordedAt: time.Now().UTC(),
		}
		if payload, err := blockedOnSiblingComment(state); err == nil {
			comment += "\n\n" + payload
		}
	}

	reviewDecision, err := nativeReviewDecision(posted.Decision)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	reviewToken, err := providerToken(capability.GitHubPRReview)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Dispatch by routed repo kind. Constructing a GitHub provider here posted
	// the native review to api.github.com for a Gitea-routed PR and failed the
	// stage with a 401 — the last GitHub hardcode on the publish path. Gitea
	// declares pr.review.submit and implements SubmitPullRequestReview, so the
	// native review works on either forge. The review identity is deliberately
	// its own capability (github:pr:review) so it can be a distinct token from
	// the PR author's; on a single-identity instance the self-review degradation
	// below still applies.
	reviewProvider, err := remediationStageProviderWithRecorder(root, repo, reviewToken, false, sidecarMutationRecorder{kind: "pr"})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if _, err := reviewProvider.SubmitPullRequestReview(ctx, providers.PullRequestReviewRequest{
		Repository: repo,
		PullID:     strconv.Itoa(selectedNumber),
		CommitSHA:  current.HeadSHA,
		Decision:   reviewDecision,
		Body:       comment,
	}); err != nil {
		// #870: on a single-GitHub-identity instance the review token is also
		// the PR's author, and GitHub categorically refuses a self-authored
		// native Review — which is every daemon-authored PR here. The native
		// Review is not a merge prerequisite: merge-pr reads the verdict from
		// the comment/label handoff posted below (the verdict-json payload
		// gather-pr-context recovers), never from a platform Review, and GitHub
		// would not honor a self-approval toward branch protection anyway. So
		// degrade to the comment/label handoff instead of failing the stage.
		// If a distinct review identity is ever provisioned
		// (GOOBERS_CRED_GITHUB_PR_REVIEW backed by a second token), this call
		// simply succeeds and no degradation happens.
		selfReview := providers.IsSelfReviewError(err)
		if !selfReview && providers.IsFineGrainedPATReviewNotFoundError(err) {
			reviewAuthor, authorErr := reviewProvider.AuthenticatedLogin(ctx)
			if authorErr != nil {
				return failProviderStage(stderr, "resolve native review author", authorErr, resultFile)
			}
			selfReview = current.Author != "" && strings.EqualFold(reviewAuthor, current.Author)
		}
		if !selfReview {
			return failProviderStage(stderr, fmt.Sprintf("submit native review for PR #%d", selectedNumber), err, resultFile)
		}
		pf(stdout, "native review skipped for PR #%d: reviewing identity authored the PR (GitHub refuses self-review) — publishing verdict via comment/label handoff instead\n", selectedNumber)
	}

	if posted.Decision == apiv1.VerdictPass {
		if err := reconcileMergeReviewStatusCommentAs(ctx, prProvider, repo, selectedNumber, verdictAuthor, comment); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("post verdict comment to PR #%d", selectedNumber), err, resultFile)
		}
		pf(stdout, "approved PR #%d at %s\n", selectedNumber, current.HeadSHA)
		return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(posted.Decision), verdictAuthor, stderr)
	}

	// Publish the native review first. If the legacy handoff below fails, the
	// absence of an exclusion label leaves the PR eligible for a later
	// merge-review run instead of stranding it without a platform verdict.
	update := providers.UpdateWorkItemRequest{
		Repository:   repo,
		ID:           strconv.Itoa(selectedNumber),
		AddLabels:    addLabels,
		RemoveLabels: removeLabels,
	}
	if _, err := provider.UpdateWorkItem(ctx, update); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("apply verdict to PR #%d", selectedNumber), err, resultFile)
	}
	if err := reconcileMergeReviewStatusCommentAs(ctx, prProvider, repo, selectedNumber, verdictAuthor, comment); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("post verdict comment to PR #%d", selectedNumber), err, resultFile)
	}
	if posted.Decision == apiv1.VerdictFail && hasAnyLabel(current.Labels, []string{remediationEscalatedLabel}) {
		if err := refreshEscalationSnapshotAfterRepeatFail(ctx, prProvider, repo, current, statusComments); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("refresh merge-escalation snapshot for PR #%d", selectedNumber), err, resultFile)
		}
	}

	priorityDispatchRequested := false
	if label == blockedOnSiblingLabel {
		// #952: publish the blocker record first so the re-tick's selector can
		// rank the elected predecessor from durable state.
		if _, err := writePriorityTriggerRequest(l.SchedulerDir(), providerGaggle(), workflowName, runID); err != nil {
			pf(stderr, "error: queue crowned-lander priority dispatch: %v\n", err)
			return 1
		}
		priorityDispatchRequested = true
		pf(stdout, "queued an immediate %s re-tick so the crowned lander is selected without waiting for the next poll\n", workflowName)
	}

	if escalationSuppressedRemediation {
		pf(stdout, "published %s verdict for PR #%d without re-applying %s because %s is still active\n",
			posted.Decision, selectedNumber, needsRemediationLabel, remediationEscalatedLabel)
	} else {
		pf(stdout, "applied %s to PR #%d (%s)\n", label, selectedNumber, posted.Decision)
	}
	return writeApplyVerdictResultWithPriorityDispatch(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(posted.Decision), verdictAuthor, priorityDispatchRequested, stderr)
}

// verdictEscalationStillBlocks reads the merge-escalation self-heal state for
// the routed provider. escalationStillBlocks already takes remediationProvider
// and reaches only ListComments/BranchTipSHA/UpdateComment, all of which
// GiteaProvider implements, so the old *GitHubProvider type assertion here was
// friction rather than a real capability boundary: it failed the whole
// apply-verdict stage on Gitea ("provider \"gitea\" does not support
// merge-escalation state") at the very last step before the verdict would have
// been published.
func verdictEscalationStillBlocks(ctx context.Context, provider providers.Provider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	prProvider, ok := provider.(remediationProvider)
	if !ok {
		return false, fmt.Errorf("provider %q does not support merge-escalation state", repo.Provider)
	}
	return escalationStillBlocks(ctx, prProvider, repo, pr)
}

func applyAdvisoryVerdict(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	selectedNumber int,
	selectedNumberStr, selectedHeadSHA, selectedBaseSHA string,
	verdict apiv1.Verdict,
	runID, resultFile string, publishAdvisory bool,
	stdout, stderr io.Writer,
) int {
	current, err := provider.GetPullRequest(ctx, repo, selectedNumberStr)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("get pull request #%d", selectedNumber), err, "")
	}
	if current.State != "open" || current.Merged {
		pln(stdout, "PR is no longer open (merged/closed since selection) — verdict moot, nothing to apply")
		return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, "moot", "", stderr)
	}
	if hasAnyLabel(current.Labels, []string{noMergeReviewLabel}) {
		pln(stdout, "PR opted out of merge-review since selection — verdict moot, nothing to apply")
		return writeApplyVerdictResultWithReason(
			resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, "moot", "",
			"PR carries "+noMergeReviewLabel, stderr,
		)
	}
	if reason := verdictPinVoidReason(verdict, selectedHeadSHA, selectedBaseSHA, current.HeadSHA, current.BaseSHA); reason != "" {
		pf(stdout, "verdict void for PR #%d: %s — skipping, will re-review next cycle\n", selectedNumber, reason)
		return writeApplyVerdictResultWithReason(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, "moot", "", reason, stderr)
	}
	if _, err := nativeReviewDecision(verdict.Decision); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if !publishAdvisory {
		pf(stdout, "advisory %s verdict for PR #%d retained locally; public publication disabled by policy\n", verdict.Decision, selectedNumber)
		return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(verdict.Decision), "", stderr)
	}

	verdict.HeadSHA = selectedHeadSHA
	verdict.BaseSHA = selectedBaseSHA
	if verdict.Digest == "" {
		verdict.Digest = providerInput("reviewDigest", "")
	}
	if verdict.SourceRunID == "" {
		verdict.SourceRunID = runID
	}
	comment := renderScopeGateStateComment(
		renderVerdictComment(verdict),
		providerInput("scopeGateParked", "") == "true",
	)
	verdictAuthor, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return failProviderStage(stderr, "resolve merge-review verdict author", err, resultFile)
	}
	if err := reconcileMergeReviewStatusCommentAs(ctx, provider, repo, selectedNumber, verdictAuthor, comment); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("post advisory verdict comment to PR #%d", selectedNumber), err, resultFile)
	}
	pf(stdout, "published advisory %s verdict for PR #%d at %s; no remediation or merge action taken\n",
		verdict.Decision, selectedNumber, current.HeadSHA)
	return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(verdict.Decision), verdictAuthor, stderr)
}

// reconcileMergeReviewStatusComment keeps the oldest marked comment authored
// by the provider's authenticated identity as the canonical status, then
// removes its marked duplicates. Relisting after every create/update makes
// concurrent creators observe and collapse each other's comments; duplicate
// deletion tolerates another reconciler winning the race.
func reconcileMergeReviewStatusComment(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prNumber int, body string) error {
	author, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return fmt.Errorf("resolve merge-review status author: %w", err)
	}
	return reconcileMergeReviewStatusCommentAs(ctx, provider, repo, prNumber, author, body)
}

// markMergeReviewVerdictStale rewrites the standing merge-review status comment
// to say its verdict no longer stands, naming why.
//
// It edits the existing comment rather than posting a new one, so a PR whose
// head moves repeatedly does not accumulate a comment per voided cycle. If no
// status comment exists yet there is nothing to invalidate and this is a no-op:
// posting "the verdict is stale" on a PR that never had a verdict would be
// noise, not information.
func markMergeReviewVerdictStale(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prNumber int, reason string) error {
	author, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return fmt.Errorf("resolve authenticated login: %w", err)
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(prNumber))
	if err != nil {
		return fmt.Errorf("list merge-review status comments: %w", err)
	}
	marked := mergeReviewStatusComments(comments, author)
	if len(marked) == 0 {
		return nil
	}
	body := fmt.Sprintf(
		"%s\n**merge-review verdict: stale**\n\nThe last published verdict no longer stands: %s.\n\n"+
			"No current review stands. merge-review will re-review this PR on its next cycle; "+
			"findings from the superseded review may already be resolved and should not be acted on.",
		mergeReviewStatusMarker, reason)
	if err := provider.UpdateComment(ctx, repo, marked[0].ID, body); err != nil {
		return fmt.Errorf("update merge-review status comment: %w", err)
	}
	return nil
}

func reconcileMergeReviewStatusCommentAs(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prNumber int, author, body string) error {
	id := strconv.Itoa(prNumber)
	comments, err := provider.ListComments(ctx, repo, id)
	if err != nil {
		return fmt.Errorf("list merge-review status comments: %w", err)
	}
	marked := mergeReviewStatusComments(comments, author)
	if len(marked) == 0 {
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo,
			ID:         id,
			Comment:    body,
		}); err != nil {
			return fmt.Errorf("create merge-review status comment: %w", err)
		}
	} else if err := provider.UpdateComment(ctx, repo, marked[0].ID, body); err != nil {
		return fmt.Errorf("update merge-review status comment: %w", err)
	}

	comments, err = provider.ListComments(ctx, repo, id)
	if err != nil {
		return fmt.Errorf("relist merge-review status comments: %w", err)
	}
	marked = mergeReviewStatusComments(comments, author)
	if len(marked) == 0 {
		return fmt.Errorf("merge-review status comment disappeared during reconciliation")
	}
	if marked[0].Body != body {
		if err := provider.UpdateComment(ctx, repo, marked[0].ID, body); err != nil {
			return fmt.Errorf("update canonical merge-review status comment: %w", err)
		}
	}
	for _, duplicate := range marked[1:] {
		if err := provider.DeleteComment(ctx, repo, duplicate.ID); err != nil {
			return fmt.Errorf("delete duplicate merge-review status comment %s: %w", duplicate.ID, err)
		}
	}
	return nil
}

func mergeReviewStatusComments(comments []providers.Comment, author string) []providers.Comment {
	marked := make([]providers.Comment, 0, len(comments))
	for _, comment := range comments {
		if isTrustedMergeReviewAuthor(comment.Author, author) && isMergeReviewStatusComment(comment.Body) {
			marked = append(marked, comment)
		}
	}
	return marked
}

func isTrustedMergeReviewAuthor(commentAuthor, authenticatedAuthor string) bool {
	return authenticatedAuthor != "" && strings.EqualFold(commentAuthor, authenticatedAuthor)
}

func isMergeReviewStatusComment(body string) bool {
	return body == mergeReviewStatusMarker || strings.HasPrefix(body, mergeReviewStatusMarker+"\n")
}

// WHAT ELECTION MEANS. Being elected does not mean "merge this regardless of
// review". It means "stop counting those siblings as blockers." And once that
// is said out loud, the verdict follows deterministically rather than by fiat:
// every finding was a pure ordering ask (allCrossPRBlocked — the PR is
// individually fine and merely waiting its turn), and this PR is the one whose
// turn it is. There is no defect left to fix, so there is nothing for
// `needs-changes` to describe. The decision is derived, not overridden. The
// same reasoning now also covers a PR whose reviewer verdict is ALREADY a
// genuine pass (resolveElectionOutcome below): sharing a deterministic
// overlap with a live sibling means it is not automatically its cluster's
// lander either, and #1071 requires that to be resolved before it reaches
// merge-pr, exactly like the needs-changes case.
//
// WHY NOT THE PREVIOUS SHAPE. elect-gate's pass branch used to route straight
// to merge-pr, deliberately bypassing this stage — which produced three
// problems at once:
//
//  1. merge-pr builds its commit message from a `pass` verdict comment pinned
//     to the current head/base SHA (structuredMergeCommitMessage, mergepr.go).
//     The bypass means no verdict comment is ever posted on this path and the
//     verdict was needs-changes anyway, so that lookup finds nothing and
//     merge-pr exits 1 — a hard stage failure, every cycle, for as long as the
//     cluster exists. The elected path could not actually merge anything.
//  2. merge-pr's "was this reviewed favorably" conjunct compares against the
//     workflow's hardcoded `verdict: "pass"` input rather than the real
//     verdict, so on this path the safety check was satisfied by a constant
//     string.
//  3. An about-to-merge PR published no verdict at all, so nothing recorded
//     why it merged.
//
// Deriving the pass here fixes all three by construction: the ordinary
// apply-verdict -> published-verdict -> merge-pr path now carries a real,
// SHA-pinned pass verdict comment, and no separate merge authority exists.
// It also costs no extra cycle — the PR still merges on this pass.
//
// Requiring an independent `pass` verdict instead would deadlock the exact
// situation election exists to break: mutually-blocked PRs cannot each earn a
// pass while each is waiting on the other.
//
// The findings are deliberately left intact on the published verdict. The
// ordering asks were real observations and stay visible; only the decision they
// rolled up to changes, and the rationale states exactly why.

// resolveElectionOutcome resolves single-lander election for a sibling-
// overlap PR (#1071/PRL-021), regardless of whether the reviewer's raw
// decision was needs-changes (electedLanderPass's original case, an
// all-ordering verdict resolved into a derived pass) or already a genuine
// pass (a reviewer verdict with no defect of its own). Either way, once a PR
// shares a deterministic file overlap with a live sibling, it must not reach
// merge-pr as a landing authority until this same election crowns it —
// otherwise two overlap-cluster members could each independently earn a
// clean pass and race GitHub's native merge queue with no arbitration at
// all, exactly the bypass #1071 exists to close.
//
// Returns rationale == "" whenever nothing needs to change: no overlap, a
// non-electable decision (fail, or an already-escalated verdict), or an
// ordinary parked needs-changes member (unrelated to this PR — the existing
// blocked-on-sibling routing already handles it via effective.Findings/
// verdictLabel without touching Decision/Rationale here).
func resolveElectionOutcome(selectedNumber int, decision apiv1.VerdictDecision, findings []apiv1.Finding, rationale string, overlappingSiblings []int, demoted map[int]bool, policy electionPolicyFunc, policyName string) (elected bool, newRationale string) {
	if len(overlappingSiblings) == 0 {
		return false, ""
	}
	if decision != apiv1.VerdictPass && decision != apiv1.VerdictNeedsChanges {
		return false, ""
	}
	if electionDecision(findings, selectedNumber, policy, demoted) {
		return true, electedLanderPassRationale(selectedNumber, apiv1.Verdict{Findings: findings, Rationale: rationale}, policyName)
	}
	if decision == apiv1.VerdictPass {
		// A genuinely clean review still is not this cluster's lander yet —
		// it must wait behind its live predecessor(s) rather than reach
		// merge-pr with nothing recording that it skipped the queue.
		return false, notElectedBlockedRationale(selectedNumber, findings, policyName)
	}
	return false, ""
}

// notElectedBlockedRationale explains why an individually-passing PR is
// nonetheless parked blocked-on-sibling: it shares a deterministic overlap
// with a live predecessor and single-lander election has not crowned it yet
// — the mirror image of electedLanderPassRationale.
func notElectedBlockedRationale(selectedNumber int, findings []apiv1.Finding, policyName string) string {
	blockers := unionBlockingPRs(findings)
	rendered := make([]string, 0, len(blockers))
	for _, b := range blockers {
		rendered = append(rendered, "#"+strconv.Itoa(b))
	}
	return fmt.Sprintf(
		"This pull request's own review found no defect, but it deterministically overlaps sibling PR(s) %s and is not yet its cluster's elected lander (policy: %s). Landing must wait for single-lander election (#1071) rather than race GitHub's native merge queue.",
		strings.Join(rendered, ", "), policyName)
}

// electedLanderPassRationale explains a derived pass in the published comment.
// A reader must be able to see that the decision changed, that a deterministic
// rule changed it, and which rule — never discover a `pass` on a PR whose
// findings all say "blocked".
func electedLanderPassRationale(selectedNumber int, posted apiv1.Verdict, policyName string) string {
	blockers := unionBlockingPRs(posted.Findings)
	rendered := make([]string, 0, len(blockers))
	for _, b := range blockers {
		rendered = append(rendered, "#"+strconv.Itoa(b))
	}
	out := fmt.Sprintf(
		"Elected lander (policy: %s). Every finding on this pull request is a pure cross-PR ordering ask against %s — no defect in this change itself — and this pull request is the one elected to go first, so those siblings no longer block it. The reviewer's `needs-changes` was entirely about waiting its turn; it is now its turn.",
		policyName, strings.Join(rendered, ", "))
	if r := strings.TrimSpace(posted.Rationale); r != "" {
		out += "\n\nOriginal reviewer rationale:\n\n> " + strings.ReplaceAll(r, "\n", "\n> ")
	}
	return out
}

// resolvedIssuePattern matches every way a goober-authored PR body states the
// issue it exists to resolve. It is DELIBERATELY broader than
// closingKeywordPattern (postmerge.go), which matches only GitHub's own
// closing-keyword grammar.
//
// The two must not be merged. closingKeywordPattern drives real mutations —
// post-merge closes exactly those issues when a PR lands — so broadening it
// would close issues a PR never claimed to close. This pattern only ever
// decides "is the work this PR describes already obsolete", and closes nothing.
//
// The extra verb is not speculative: `goobers open-pr` writes its body as
// "Implements #N: **title**." (openprbody.go), which is not a GitHub closing
// keyword — by design, since post-merge does the closing explicitly rather than
// letting GitHub do it. So the single most common goober PR body form is
// invisible to closingKeywordPattern. PR #919 was exactly that shape, and a
// mootness check reading only closing keywords would have missed the case this
// whole path exists for.
var resolvedIssuePattern = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|implement(?:s|ed)?)\s+#(\d+)`)

// resolvedIssueNumbers extracts every distinct issue number a PR body claims to
// resolve, in first-seen order.
func resolvedIssueNumbers(body string) []string {
	matches := resolvedIssuePattern.FindAllStringSubmatch(body, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// mootFailReason reports whether a `fail`-verdicted pull request is MOOT —
// work that should never have been done, as opposed to work done wrongly — and
// the human-readable reason it is.
//
// The distinction matters because it decides who has to act. `fail` normally
// means the reviewer judged the APPROACH wrong, which is a genuine judgment
// call reserved for a human (design doc §4 D2), and auto-closing those would
// take a person out of the one loop they were deliberately left in. But a
// meaningful share of `fail` verdicts are not that at all: the work was already
// obsolete before the run started, and there is nothing for anyone to decide.
//
// PR #919 (weekend_10, 2026-07-19) is the worked example. #827 merged the real
// torn-read fix at 2026-07-18T10:57Z; issue #684 was closed as superseded at
// 02:52Z; the implementation run opened #919 for #684 at 03:34Z, 42 minutes
// AFTER its issue was closed. The reviewer's rationale said outright "close PR
// #919 rather than merging it" — and had no mechanism to do so, so it sat open
// until a human closed it by hand. (#947 tracks preventing the wasted run in
// the first place; this only stops the debris needing a human.)
//
// Mootness is established ONLY by a deterministic fact about the repository,
// never by the reviewer's prose. The verdict being `fail` is what makes the
// question worth asking; it is not itself evidence of the answer. A model can
// be wrong about whether something is superseded, and closing a pull request on
// a wrong belief is not a failure mode worth accepting to save a click — so the
// model's rationale gates nothing here, it is merely quoted in the comment.
//
// Fails closed in every ambiguous case: a provider error, an unresolvable
// issue, or a pull request that references no issue at all all return false and
// take the ordinary escalate-to-a-human path.
func mootFailReason(ctx context.Context, provider providers.Provider, repo providers.RepositoryRef, pr *providers.PullRequestSummary) (string, bool) {
	// Condition 1: the pull request no longer changes anything. Whatever it
	// proposed is already contained in its base, so there is nothing to merge
	// and nothing to decide. This is the general "already fixed elsewhere"
	// shape, independent of any issue bookkeeping.
	files, err := provider.PullRequestFiles(ctx, repo, strconv.Itoa(pr.Number))
	if err == nil && len(files) == 0 {
		return "its diff against the base is now empty — whatever it proposed is already contained in the base branch", true
	}

	// Condition 2: every issue this pull request exists to resolve is itself
	// already closed. One unresolvable or still-open issue is enough to make
	// this NOT moot: the pull request may still be the thing that closes it.
	issues := resolvedIssueNumbers(pr.Body)
	if len(issues) == 0 {
		return "", false
	}
	for _, id := range issues {
		item, err := provider.GetWorkItem(ctx, repo, id)
		if err != nil {
			return "", false
		}
		if !strings.EqualFold(item.State, "closed") {
			return "", false
		}
	}
	return fmt.Sprintf("every issue it exists to close (%s) is already closed", strings.Join(prefixedIssueNumbers(issues), ", ")), true
}

// prefixedIssueNumbers renders issue IDs as #N for a human-facing comment.
func prefixedIssueNumbers(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "#"+id)
	}
	return out
}

// duplicateOfEarlierPR reports whether pr is a true duplicate that should be
// closed rather than escalated (#987): another OPEN goober PR with a LOWER
// number already references one of the same issues. The first PR to claim an
// issue wins — fifo, consistent with lander election — so this later one is
// redundant and can never both-land (the #966/#969 deadlock). Best-effort: a
// listing failure returns not-a-duplicate rather than fabricating a close.
// The caller must gate this to non-passing PRs, so a passing PR is never
// closed as a duplicate.
func duplicateOfEarlierPR(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr *providers.PullRequestSummary) (string, bool) {
	mine := referencedIssueNumbers(pr.Body)
	if len(mine) == 0 {
		return "", false
	}
	mineSet := make(map[string]bool, len(mine))
	for _, id := range mine {
		mineSet[id] = true
	}
	others, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, HeadPrefix: providerBranchNamespace(), SkipCheckState: true,
	})
	if err != nil {
		return "", false
	}
	for _, o := range others {
		// ListPullRequests returns only open PRs (the same list #414's open-PR
		// backstop relies on); a lower number is a strictly-earlier claim.
		if o.Number >= pr.Number {
			continue
		}
		for _, oid := range referencedIssueNumbers(o.Body) {
			if mineSet[oid] {
				return fmt.Sprintf("pull request #%d already implements the same issue #%s and was opened first", o.Number, oid), true
			}
		}
	}
	return "", false
}

// supersededByIdenticalSibling reports whether pr is a byte-identical duplicate
// of an EARLIER open sibling that should be closed as superseded (#1211) — the
// gap duplicateOfEarlierPR does not cover, because that keys on a shared issue
// and two independently-claimed issues can converge to the identical tree
// (#1179/#1180: two issues, same four files byte-for-byte). The earlier PR wins,
// fifo, exactly as it does for a shared-issue duplicate; this later one is
// redundant and can never both-land, deadlocking its cluster.
//
// Supersession is established ONLY by a deterministic repository fact: the two
// pull requests propose the byte-identical diff — the identical set of changed
// files, each with the byte-identical patch. The reviewer's prose gates nothing
// (it is only quoted in the close comment), matching mootFailReason's contract.
// Deliberately conservative: it requires the patches to match verbatim, so two
// duplicates opened against DIFFERENT bases (whose hunk line numbers differ)
// are NOT auto-closed but fall through to the ordinary human-escalation path —
// the safe direction for an action that closes a pull request. The caller gates
// this to non-passing PRs, so a passing PR is never closed as superseded.
//
// Fails closed on any provider error and on any file whose patch GitHub omits
// (binary or over its size cutoff — byte-identity is then unverifiable).
func supersededByIdenticalSibling(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr *providers.PullRequestSummary) (string, bool) {
	mine, ok := changedDiffDigest(ctx, provider, repo, pr.Number)
	if !ok {
		return "", false
	}
	others, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, HeadPrefix: providerBranchNamespace(), SkipCheckState: true,
	})
	if err != nil {
		return "", false
	}
	for _, o := range others {
		// ListPullRequests returns only open PRs; a lower number is a strictly-
		// earlier claim, so only it can supersede pr (never the reverse).
		if o.Number >= pr.Number {
			continue
		}
		theirs, ok := changedDiffDigest(ctx, provider, repo, o.Number)
		if !ok {
			continue
		}
		if theirs == mine {
			return fmt.Sprintf(
				"pull request #%d proposes a byte-identical diff and was opened first — this one is a redundant duplicate",
				o.Number), true
		}
	}
	return "", false
}

// changedDiffDigest returns a deterministic digest of a pull request's diff —
// every changed file's path, status, and exact patch text, in a
// listing-order-independent way — and whether it could be computed reliably.
// It returns ok=false when the provider errors, the PR has an empty diff
// (nothing to compare; mootFailReason owns that case), or ANY file's patch is
// omitted by the provider (binary/too-large): an unverifiable file must never
// be treated as matching, so callers fail closed on it.
func changedDiffDigest(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, number int) (string, bool) {
	files, err := provider.PullRequestFiles(ctx, repo, strconv.Itoa(number))
	if err != nil || len(files) == 0 {
		return "", false
	}
	sorted := append([]providers.ChangedFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		if f.Patch == "" {
			return "", false
		}
		// Length-prefix every field so no path/status/patch boundary can be
		// forged by concatenation (e.g. "a"+"b" colliding with "ab").
		_, _ = fmt.Fprintf(h, "%d:%s\n%d:%s\n%d:%s\n",
			len(f.Path), f.Path, len(f.Status), f.Status, len(f.Patch), f.Patch)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), true
}

// closeMootPullRequest closes a pull request that is no longer needed, stating
// both the objective reason and the reviewer's own rationale.
//
// Both are included on purpose. The objective reason is what justifies closing
// automatically and is the part a reader should be able to check; the rationale
// is the reviewer's reasoning and is the part that explains it. Publishing only
// the second would make an automated close look like it rests on a model's
// opinion, which is precisely what it does not rest on.
//
// No native review is submitted first: a changes-requested review on a pull
// request being closed in the same breath is noise, and #870 means it would
// frequently be refused as a self-review anyway.
func closeMootPullRequest(ctx context.Context, provider providers.Provider, repo providers.RepositoryRef, selectedNumber int, current *providers.PullRequestSummary, verdict apiv1.Verdict, reason, resultFile string, stdout, stderr io.Writer) int {
	comment := fmt.Sprintf(
		"Closing this pull request automatically: %s.\n\nThis change is **no longer needed** rather than wrong — there is no decision for a human to make. Reopen it if that reading is incorrect.\n\n> %s",
		reason, strings.ReplaceAll(strings.TrimSpace(verdict.Rationale), "\n", "\n> "))
	if _, err := provider.ClosePullRequest(ctx, providers.ClosePullRequestRequest{
		Repository: repo,
		PullID:     strconv.Itoa(selectedNumber),
		Comment:    comment,
	}); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("close moot pull request #%d", selectedNumber), err, resultFile)
	}
	pf(stdout, "closed moot PR #%d: %s\n", selectedNumber, reason)
	return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, "closed-moot", "", stderr)
}

func currentPullRequest(ctx context.Context, provider providers.Provider, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error) {
	if githubProvider, ok := provider.(*providers.GitHubProvider); ok {
		return githubProvider.GetPullRequest(ctx, repo, pullID)
	}
	current, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{
		Repository: repo,
		PullID:     pullID,
	})
	if err != nil {
		return providers.PullRequestSummary{}, err
	}
	return providers.PullRequestSummary{
		ID:        pullID,
		Number:    current.Number,
		State:     current.State,
		Merged:    current.Merged,
		HeadSHA:   current.HeadSHA,
		BaseSHA:   current.BaseSHA,
		Body:      current.Body,
		Labels:    current.Labels,
		Integrity: current.Integrity,
	}, nil
}

// newApplyVerdictProviderForRepo builds apply-verdict's forge client through the
// shared merge-review/stage provider seam.
//
// It used to hand-roll each arm, and its GitHub arm called
// newCachedGitHubProvider directly. That skipped newGitHubProviderForStage and
// therefore skipped providers.WithConfiguredLogin, so under GitHub App auth the
// provider had no declared identity and AuthenticatedLogin — which apply-verdict
// calls to reconcile its own trusted verdict/status comments — fell back to
// GET /user. Installation tokens cannot call that endpoint, so every
// merge-review run died with "Resource not accessible by integration" and the
// scheduler opened its per-workflow auth circuit (#3885). #3343/#3344 fixed the
// shared seam but missed this constructor; routing through the seam is what
// keeps the two from drifting apart again.
//
// Per-arm options reproduce the previous behavior exactly: the GitHub arm stays
// conditional-GET cached with no mutation recorder, the Gitea arm keeps its
// kind="pr" recorder and is uncached, and the ADO arm resolves its own
// credential inside newADOProviderForStage (no capability token is minted for
// it here, same as before).
func newApplyVerdictProviderForRepo(root string, repo providers.RepositoryRef) (providers.Provider, error) {
	opts := []stageProviderOption{withStageProviderCapability(capability.ProviderPRWrite)}
	switch repo.Provider {
	case providers.ProviderADO:
	case providers.ProviderGitea:
		opts = append(opts, withStageProviderMutations("pr"))
	case providers.ProviderGitHub:
		opts = append(opts, withStageProviderCache())
	default:
		return nil, fmt.Errorf("apply-verdict does not support repository provider %q", repo.Provider)
	}
	return newMergeReviewProvider(root, repo, false, opts...)
}

// publishADOPassVerdict publishes a PASS merge-review verdict on Azure DevOps.
// ADO has neither a native self-review to submit nor the GitHub
// sticky-comment/label verdict transport (the GitHub path's
// UpdateWorkItem(ID: PR#) would address the unrelated work item that shares the
// PR's numeric id — the wrong-object hazard), so the verdict rides on a
// provider-native PR status (genre "goobers", name "validation" — the same
// surface report-pr-status publishes) that an ADO status-check branch policy can
// gate on. It emits decision=pass into the result file so merge-review's
// published-verdict gate advances to merge-pr. See the ADO merge epic (#2061).
func publishADOPassVerdict(
	ctx context.Context,
	provider providers.PullRequestStatusPublisher,
	repo providers.RepositoryRef,
	selectedNumber int,
	current providers.PullRequestSummary,
	resultFile string,
	stdout, stderr io.Writer,
) int {
	if _, err := provider.PublishPullRequestStatus(ctx, providers.PullRequestStatusRequest{
		Repository:  repo,
		PullID:      strconv.Itoa(selectedNumber),
		Genre:       "goobers",
		Name:        "validation",
		State:       providers.CheckStatePassing,
		Description: "goobers merge-review verdict: pass",
	}); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("publish pass verdict status for PR #%d", selectedNumber), err, resultFile)
	}
	pf(stdout, "approved PR #%d at %s via goobers/validation PR status\n", selectedNumber, current.HeadSHA)
	return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(apiv1.VerdictPass), "", stderr)
}

// publishADONonPassVerdict publishes a non-pass (needs-changes or fail)
// merge-review verdict on Azure DevOps and hands it off to the pr-remediation
// loop — the symmetric counterpart to publishADOPassVerdict. ADO has neither a
// native changes-requested review to submit nor the GitHub sticky-comment/label
// verdict transport (the GitHub path's UpdateWorkItem(ID: PR#) would address the
// unrelated work item that shares the PR's numeric id — the wrong-object
// hazard), so the verdict rides on three provider-native surfaces, each the ADO
// analog of a GitHub handoff channel:
//
//  1. A failed goobers/validation PR status (the same surface publishADOPassVerdict
//     and report-pr-status publish) an ADO status-check branch policy gates the
//     merge on. needs-changes and fail BOTH publish CheckStateFailing ("failed"):
//     the PR must not land until reworked, and a status genre cannot carry the
//     needs-changes/fail split — the remediation label below is the routing
//     signal, exactly as GitHub carries it in verdictLabel.
//  2. The goobers:needs-remediation PR label, written via the native ADO
//     PR-labels endpoint (AddPullRequestLabels), NEVER UpdateWorkItem(ID: PR#).
//     ListPullRequests already surfaces ADO PR labels, so pr-remediation's
//     existing selector (remediationPriorityFor) fires on it unmodified.
//  3. The findings + verdict-json machine payload posted as a PR thread comment
//     (PostPullRequestThreadComment) — the ADO analog of the GitHub sticky
//     status comment. gather-pr-context reads this thread back to recover the
//     findings the remediating agent works from, so the verdict is SHA-pinned to
//     the reviewed head/base (verified equal to the deterministic pin by
//     verdictPinVoidReason above) before rendering.
//
// It emits the decision into the result file so merge-review's published-verdict
// gate routes away from merge (any non-pass decision terminates), and returns 0
// so the run completes cleanly instead of the old hard-fail. See the ADO merge
// epic (#2061).
func publishADONonPassVerdict(
	ctx context.Context,
	provider *providers.ADOProvider,
	repo providers.RepositoryRef,
	selectedNumber int,
	current providers.PullRequestSummary,
	verdict apiv1.Verdict,
	resultFile string,
	stdout, stderr io.Writer,
) int {
	pullID := strconv.Itoa(selectedNumber)
	if _, err := provider.PublishPullRequestStatus(ctx, providers.PullRequestStatusRequest{
		Repository:  repo,
		PullID:      pullID,
		Genre:       "goobers",
		Name:        "validation",
		State:       providers.CheckStateFailing,
		Description: "goobers merge-review verdict: " + string(verdict.Decision),
	}); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("publish %s verdict status for PR #%d", verdict.Decision, selectedNumber), err, resultFile)
	}
	// Route the label by decision, mirroring the GitHub verdictLabel contract
	// (§4 D2): a fail escalates for a human (goobers:merge-escalated) and is
	// NEVER burned on the remediation budget, while needs-changes routes to
	// remediation (goobers:needs-remediation). Clearing needs-remediation on a
	// fail — and declining to re-arm it while an escalation still blocks — is
	// what parks a stuck PR for a human instead of looping it forever.
	label := verdictLabel(verdict.Decision, verdict.Findings)
	var addLabels, removeLabels []string
	switch label {
	case remediationEscalatedLabel:
		addLabels = []string{remediationEscalatedLabel}
		removeLabels = []string{needsRemediationLabel}
	case needsRemediationLabel:
		// Verdict-side escalation suppression (behavior 3d): if the PR already
		// carries an active escalation, keep it parked — clear any stale
		// needs-remediation rather than pulling it back into the budget.
		names, err := provider.PullRequestLabelNames(ctx, repo, pullID)
		if err != nil {
			return failProviderStage(stderr, fmt.Sprintf("read labels for PR #%d", selectedNumber), err, resultFile)
		}
		if hasAnyLabel(names, []string{remediationEscalatedLabel}) {
			removeLabels = []string{needsRemediationLabel}
		} else {
			addLabels = []string{needsRemediationLabel}
		}
	default:
		// blocked-on-sibling has no ADO analogue (no sibling election); route it
		// to remediation as the prior behavior did.
		addLabels = []string{needsRemediationLabel}
	}
	if len(addLabels) > 0 {
		if err := provider.AddPullRequestLabels(ctx, repo, pullID, addLabels); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("apply %v label to PR #%d", addLabels, selectedNumber), err, resultFile)
		}
	}
	for _, name := range removeLabels {
		if err := provider.RemovePullRequestLabel(ctx, repo, pullID, name); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("clear %s label on PR #%d", name, selectedNumber), err, resultFile)
		}
	}
	// SHA-pin the published verdict to the reviewed state (== the deterministic
	// pin, checked above) so gather-pr-context can trust the head/base it reads
	// back from the thread, mirroring the GitHub path's posted.HeadSHA/BaseSHA.
	verdict.HeadSHA = current.HeadSHA
	verdict.BaseSHA = current.BaseSHA
	if _, err := provider.PostPullRequestThreadComment(ctx, repo, pullID, renderVerdictComment(verdict)); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("post verdict thread comment to PR #%d", selectedNumber), err, resultFile)
	}
	pf(stdout, "published %s verdict for PR #%d at %s via goobers/validation PR status, labels %v (cleared %v), and PR thread\n",
		verdict.Decision, selectedNumber, current.HeadSHA, addLabels, removeLabels)
	return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(verdict.Decision), "", stderr)
}

func nativeReviewDecision(decision apiv1.VerdictDecision) (providers.ReviewDecision, error) {
	switch decision {
	case apiv1.VerdictPass:
		return providers.ReviewDecisionApproved, nil
	case apiv1.VerdictNeedsChanges, apiv1.VerdictFail:
		return providers.ReviewDecisionChangesRequested, nil
	default:
		return "", fmt.Errorf("unsupported verdict decision %q", decision)
	}
}

func writeApplyVerdictResult(path string, selectedNumber int, headSHA, baseSHA, decision, verdictAuthor string, stderr io.Writer) int {
	return writeApplyVerdictResultWithPriorityDispatch(path, selectedNumber, headSHA, baseSHA, decision, verdictAuthor, false, stderr)
}

func writeApplyVerdictResultWithReason(path string, selectedNumber int, headSHA, baseSHA, decision, verdictAuthor, reason string, stderr io.Writer) int {
	return writeApplyVerdictResultWithReasonAndPriorityDispatch(path, selectedNumber, headSHA, baseSHA, decision, verdictAuthor, reason, false, stderr)
}

func writeApplyVerdictResultWithPriorityDispatch(path string, selectedNumber int, headSHA, baseSHA, decision, verdictAuthor string, priorityDispatchRequested bool, stderr io.Writer) int {
	return writeApplyVerdictResultWithReasonAndPriorityDispatch(path, selectedNumber, headSHA, baseSHA, decision, verdictAuthor, "", priorityDispatchRequested, stderr)
}

func writeApplyVerdictResultWithReasonAndPriorityDispatch(path string, selectedNumber int, headSHA, baseSHA, decision, verdictAuthor, reason string, priorityDispatchRequested bool, stderr io.Writer) int {
	advisoryMode, _ := strconv.ParseBool(providerInput("advisoryMode", "false"))
	out := map[string]string{
		"selectedNumber":            strconv.Itoa(selectedNumber),
		"selectedHeadSha":           headSHA,
		"selectedBaseSha":           baseSHA,
		"decision":                  decision,
		"verdictAuthor":             verdictAuthor,
		"advisoryMode":              strconv.FormatBool(advisoryMode),
		"priorityDispatchRequested": strconv.FormatBool(priorityDispatchRequested),
		"scopeGateParked":           providerInput("scopeGateParked", ""),
	}
	if reason != "" {
		out["reason"] = reason
	}
	data, err := json.Marshal(out)
	if err != nil {
		pf(stderr, "error: marshal verdict result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", path, err)
		return 2
	}
	return 0
}

// readLatestGateVerdict reads runID's own journal and returns the Verdict
// artifact of the LAST gate.evaluated event named gateName (last, not
// first, in case a repass re-evaluated it) — nil, nil if no such event
// exists yet.
func readLatestGateVerdict(runsDir, runID, gateName string) (*apiv1.Verdict, error) {
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		return nil, err
	}
	events, err := rd.Events()
	if err != nil {
		return nil, err
	}
	var ref *journal.Ref
	for i := range events {
		e := &events[i]
		if e.Type == journal.EventGateEvaluated && e.Gate == gateName && e.Ref != nil {
			ref = e.Ref
		}
	}
	if ref == nil {
		return nil, nil
	}
	data, err := rd.ArtifactBytes(*ref)
	if err != nil {
		return nil, fmt.Errorf("read verdict artifact: %w", err)
	}
	var v apiv1.Verdict
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("unmarshal verdict artifact: %w", err)
	}
	return &v, nil
}

// renderVerdictComment is the prose PR comment — a human-readable
// projection of the same Verdict artifact (design doc §4: "one source of
// truth, so comment and fix cannot drift"), never a separately-authored
// message. The stable mergeReviewStatusMarker identifies the comment for
// in-place updates without relying on prose. It also embeds the SAME Verdict
// as a machine-readable payload (verdictJSONComment) in an HTML comment
// appended to the end — invisible when GitHub renders the comment, but
// readable by `gather-pr-context` (issue #362), which runs in a different
// workflow's run and so has no journal/runID relationship to this run's own
// artifact. This keeps the prose and the machine payload as ONE posted
// comment (still a single source of truth) rather than growing a second,
// driftable channel.
func renderVerdictComment(v apiv1.Verdict) string {
	s := fmt.Sprintf("%s\n**merge-review verdict: %s**\n\n%s", mergeReviewStatusMarker, v.Decision, v.Summary)
	if v.Rationale != "" {
		s += "\n\n" + v.Rationale
	}
	for _, f := range v.Findings {
		line := fmt.Sprintf("\n- [%s] %s", f.Severity, f.Message)
		if f.Class != "" {
			line = fmt.Sprintf("\n- [%s/%s] %s", f.Severity, f.Class, f.Message)
		}
		if f.Location != "" {
			line += " (" + f.Location + ")"
		}
		s += line
	}
	if payload, err := verdictJSONComment(v); err == nil {
		s += "\n\n" + payload
	}
	return s
}

// verdictJSONPattern matches the machine-readable payload
// renderVerdictComment appends to its posted comment.
var verdictJSONPattern = regexp.MustCompile(`(?s)<!-- verdict-json: (.*?) -->`)

// verdictJSONComment marshals v into the HTML-comment payload
// renderVerdictComment appends to the prose comment.
func verdictJSONComment(v apiv1.Verdict) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal verdict payload: %w", err)
	}
	return fmt.Sprintf("<!-- verdict-json: %s -->", data), nil
}

const scopeGateParkedCommentMarker = "<!-- scope-gate-parked: true -->"

func renderScopeGateStateComment(comment string, parked bool) string {
	if !parked {
		return comment
	}
	return comment + "\n\n" + scopeGateParkedCommentMarker
}

func validateVerdictForPublish(v apiv1.Verdict) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal verdict payload: %w", err)
	}
	return validateVerdictJSON(data)
}

func validateVerdictJSON(data []byte) error {
	if err := validateSchemaJSON(schemas.Envelope["verdict"], data); err != nil {
		return fmt.Errorf("validate verdict payload: %w", err)
	}
	return nil
}

// parseVerdictComment recovers the Verdict a merge-review apply-verdict run
// embedded in a PR comment via verdictJSONComment — the handoff
// pr-remediation's gather-pr-context (issue #362) uses to read merge-review's
// structured verdict back from a DIFFERENT run's own journal (which has no
// artifact for it). Returns ok=false if body has no embedded payload (an
// older comment, or one not posted by apply-verdict at all) — that is a
// normal "no verdict recorded yet" outcome, not a parse error.
func parseVerdictComment(body string) (v apiv1.Verdict, ok bool) {
	m := verdictJSONPattern.FindStringSubmatch(body)
	if m == nil {
		return apiv1.Verdict{}, false
	}
	if err := json.Unmarshal([]byte(m[1]), &v); err != nil {
		return apiv1.Verdict{}, false
	}
	return v, true
}

var findingSetHistoryPattern = regexp.MustCompile(`(?s)<!-- finding-set-history: (.*?) -->`)

func findingSetHistoryComment(history findingSetHistory) (string, error) {
	data, err := json.Marshal(history)
	if err != nil {
		return "", fmt.Errorf("marshal finding-set history: %w", err)
	}
	return fmt.Sprintf("<!-- finding-set-history: %s -->", data), nil
}

func parseFindingSetHistoryComment(body string) (findingSetHistory, bool) {
	m := findingSetHistoryPattern.FindStringSubmatch(body)
	if m == nil {
		return findingSetHistory{}, false
	}
	var history findingSetHistory
	if err := json.Unmarshal([]byte(m[1]), &history); err != nil {
		return findingSetHistory{}, false
	}
	if len(history.Hashes) > findingSetHistoryLimit {
		history.Hashes = append([]string(nil), history.Hashes[len(history.Hashes)-findingSetHistoryLimit:]...)
	}
	return history, true
}

func findingSetHistoryFromComments(comments []providers.Comment, author string) (findingSetHistory, error) {
	marked := mergeReviewStatusComments(comments, author)
	if len(marked) == 0 {
		return findingSetHistory{}, nil
	}
	if history, ok := parseFindingSetHistoryComment(marked[0].Body); ok && len(history.Hashes) > 0 {
		return history, nil
	}
	priorVerdict, ok := parseVerdictComment(marked[0].Body)
	if !ok {
		return findingSetHistory{}, nil
	}
	digest, err := findingSetDigest(priorVerdict.Findings)
	if err != nil {
		return findingSetHistory{}, err
	}
	return findingSetHistory{Hashes: []string{digest}}, nil
}

func advanceFindingSetHistory(prior findingSetHistory, findings []apiv1.Finding) (findingSetHistory, string, bool, error) {
	digest, err := findingSetDigest(findings)
	if err != nil {
		return findingSetHistory{}, "", false, err
	}
	revisited := false
	if len(prior.Hashes) > 0 && prior.Hashes[len(prior.Hashes)-1] == digest {
		for _, previous := range prior.Hashes[:len(prior.Hashes)-1] {
			if previous == digest {
				revisited = true
				break
			}
		}
		return findingSetHistory{Hashes: append([]string(nil), prior.Hashes...)}, digest, revisited, nil
	}
	for _, previous := range prior.Hashes {
		if previous == digest {
			revisited = true
			break
		}
	}
	hashes := append(append([]string(nil), prior.Hashes...), digest)
	if len(hashes) > findingSetHistoryLimit {
		hashes = hashes[len(hashes)-findingSetHistoryLimit:]
	}
	return findingSetHistory{Hashes: hashes}, digest, revisited, nil
}

func findingSetDigest(findings []apiv1.Finding) (string, error) {
	encoded := make([]string, 0, len(findings))
	for _, finding := range findings {
		blockers := append([]int(nil), finding.BlockingPRs...)
		sort.Ints(blockers)
		if len(blockers) > 1 {
			unique := blockers[:1]
			for _, blocker := range blockers[1:] {
				if blocker != unique[len(unique)-1] {
					unique = append(unique, blocker)
				}
			}
			blockers = unique
		}
		data, err := json.Marshal(canonicalFinding{
			Severity:    finding.Severity,
			Class:       finding.Class,
			Message:     finding.Message,
			Location:    finding.Location,
			BlockingPRs: blockers,
		})
		if err != nil {
			return "", fmt.Errorf("marshal canonical finding: %w", err)
		}
		encoded = append(encoded, string(data))
	}
	sort.Strings(encoded)
	if len(encoded) > 1 {
		unique := encoded[:1]
		for _, finding := range encoded[1:] {
			if finding != unique[len(unique)-1] {
				unique = append(unique, finding)
			}
		}
		encoded = unique
	}
	data, err := json.Marshal(encoded)
	if err != nil {
		return "", fmt.Errorf("marshal canonical finding set: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func isTrustedMergeReviewStatusComment(commentAuthor, body, authenticatedAuthor string) bool {
	return isTrustedMergeReviewAuthor(commentAuthor, authenticatedAuthor) && isMergeReviewStatusComment(body)
}
