package main

import (
	"context"
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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// blockedOnSiblingLabel marks a PR that's correct in isolation but must wait
// behind a named sibling (#747) — see verdictLabel's doc comment.
const blockedOnSiblingLabel = "goobers:blocked-on-sibling"

const mergeReviewStatusMarker = "<!-- goobers:merge-review-status -->"

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
//   - If a real defect is present (any non-cross-pr-blocked finding), the
//     findings are returned UNCHANGED — a real bug takes priority over
//     sequencing and must route to remediation, never be merged as a lander.
//   - Otherwise, if overlappingSiblings is non-empty, a cross-pr-blocked
//     finding carrying that full set is appended, so allCrossPRBlocked /
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
	for _, f := range findings {
		if f.Class != apiv1.FindingCrossPRBlocked {
			return findings
		}
	}
	return append(findings[:len(findings):len(findings)], apiv1.Finding{
		Severity:    apiv1.SeverityWarning,
		Class:       apiv1.FindingCrossPRBlocked,
		Message:     fmt.Sprintf("deterministic file overlap with sibling PR(s) %v — sequencing required", overlappingSiblings),
		BlockingPRs: overlappingSiblings,
	})
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
func predecessorBlockers(thisPR int, blockers []int, policy electionPolicyFunc) []int {
	var out []int
	for _, b := range blockers {
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

// runApplyVerdict implements `goobers apply-verdict` (issue #359): reads the
// holistic review gate's Verdict back from this run's own journal (the gate
// already records it as an artifact via internal/gate's recordVerdict — no
// new plumbing), cross-checks its SHA echo against gather-sibling-context's
// authoritative pin, and re-checks that pin against the PR's CURRENT head/base
// before acting (design doc §6 D6: a verdict computed against a state that no
// longer exists is void, not actionable). It then publishes the verdict as a
// SHA-pinned native GitHub review. Every verdict also retains the existing prose
// comment handoff consumed by merge, cache, and remediation paths; non-pass
// verdicts additionally retain their decision labels.
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
	fs := flag.NewFlagSet("apply-verdict", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gateName := fs.String("gate", "review", "the gate name whose verdict to apply")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers apply-verdict [--gate name] [path]\n\n"+
			"Read the holistic review gate's Verdict from this run's own journal,\n"+
			"cross-check its optional SHA echo against the deterministic review\n"+
			"pin, re-check that pin against the PR's current head/base, and — if\n"+
			"still valid — post the verdict as a native GitHub review and retain\n"+
			"the PR-comment handoff. Non-pass verdicts also apply a remediation\n"+
			"label. A\n"+
			"stale SHA pin voids the verdict: no comment, no label, exit 0 (this\n"+
			"cycle's work is simply moot, not an error — merge-review re-reviews\n"+
			"next tick). Requires selectedNumber, selectedHeadSha, and\n"+
			"selectedBaseSha from Task.InputsFrom. Exit codes: 0 = applied (or\n"+
			"voided), 1 = business error, 2 = usage/IO error.\n")
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
	provider := newGitHubProvider(token)

	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", "goobers/")
	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, "")
	}
	var current *providers.PullRequestSummary
	for i := range prs {
		if prs[i].Number == selectedNumber {
			current = &prs[i]
			break
		}
	}
	if current == nil {
		pln(stdout, "PR is no longer open (merged/closed since selection) — verdict moot, nothing to apply")
		return writeApplyVerdictResult(resultFile, selectedNumber, "", "", "moot", "", stderr)
	}

	// D6: gather-sibling-context's deterministic pin is authoritative. The
	// reviewer's optional echo can disprove that it reviewed the gathered diff,
	// but omitting the echo cannot bypass the current-state check.
	if reason := verdictPinVoidReason(*verdict, selectedHeadSHA, selectedBaseSHA, current.HeadSHA, current.BaseSHA); reason != "" {
		pf(stdout, "verdict void for PR #%d: %s — skipping, will re-review next cycle\n", selectedNumber, reason)
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
		if reason, moot := mootFailReason(ctx, provider, repo, current); moot {
			return closeMootPullRequest(ctx, provider, repo, selectedNumber, current, *verdict, reason, resultFile, stdout, stderr)
		}
		if reason, dup := duplicateOfEarlierPR(ctx, provider, repo, current); dup {
			return closeMootPullRequest(ctx, provider, repo, selectedNumber, current, *verdict, reason, resultFile, stdout, stderr)
		}
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

	// Fold the deterministic file-overlap set (#990) into the findings used for
	// sequencing ROUTING only — not into the published verdict, whose findings
	// stay the reviewer's own (renderVerdictComment below reads posted, not
	// effective). This lets a green PR whose only issue is a file collision
	// reach election even if the reviewer under-named (or missed) the blocking
	// siblings; a verdict with a real defect is returned unchanged.
	overlappingSiblings := parseOverlappingSiblings(providerInput("overlappingSiblings", ""))
	effective := posted
	effective.Findings = withOverlapBackstop(posted.Findings, overlappingSiblings)

	// Election resolves an all-ordering verdict into a real pass (#833/#834,
	// reframed). See electedLanderPassRationale.
	if elected, rationale := electedLanderPass(selectedNumber, effective); elected {
		posted.Decision = apiv1.VerdictPass
		posted.Rationale = rationale
	}

	comment := renderVerdictComment(posted)
	label := verdictLabel(posted.Decision, effective.Findings)
	if label == blockedOnSiblingLabel {
		// Record only the predecessors this parked PR must wait behind, not the
		// symmetric union of every overlapping sibling (#991) — otherwise a 3+
		// cluster deadlocks (each member lists the others, none ever unparks).
		policy, _ := resolveElectionPolicy(providerInput("electionPolicy", defaultElectionPolicy))
		state := blockedOnSiblingState{
			Blockers:   predecessorBlockers(selectedNumber, unionBlockingPRs(effective.Findings), policy),
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
	reviewProvider := newGitHubProvider(reviewToken, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))
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
		if !providers.IsSelfReviewError(err) {
			return failProviderStage(stderr, fmt.Sprintf("submit native review for PR #%d", selectedNumber), err, resultFile)
		}
		pf(stdout, "native review skipped for PR #%d: reviewing identity authored the PR (GitHub refuses self-review) — publishing verdict via comment/label handoff instead\n", selectedNumber)
	}

	verdictAuthor, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return failProviderStage(stderr, "resolve merge-review verdict author", err, resultFile)
	}
	if posted.Decision == apiv1.VerdictPass {
		if err := reconcileMergeReviewStatusCommentAs(ctx, provider, repo, selectedNumber, verdictAuthor, comment); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("post verdict comment to PR #%d", selectedNumber), err, resultFile)
		}
		pf(stdout, "approved PR #%d at %s\n", selectedNumber, current.HeadSHA)
		return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(posted.Decision), verdictAuthor, stderr)
	}

	// Publish the native review first. If the legacy handoff below fails, the
	// absence of an exclusion label leaves the PR eligible for a later
	// merge-review run instead of stranding it without a platform verdict.
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         strconv.Itoa(selectedNumber),
		AddLabels:  []string{label},
	}); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("apply verdict to PR #%d", selectedNumber), err, resultFile)
	}
	if err := reconcileMergeReviewStatusCommentAs(ctx, provider, repo, selectedNumber, verdictAuthor, comment); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("post verdict comment to PR #%d", selectedNumber), err, resultFile)
	}

	pf(stdout, "applied %s to PR #%d (%s)\n", label, selectedNumber, verdict.Decision)
	return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(posted.Decision), verdictAuthor, stderr)
}

// reconcileMergeReviewStatusComment keeps the oldest marked comment authored
// by the provider's authenticated identity as the canonical status, then
// removes its marked duplicates. Relisting after every create/update makes
// concurrent creators observe and collapse each other's comments; duplicate
// deletion tolerates another reconciler winning the race.
func reconcileMergeReviewStatusComment(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, prNumber int, body string) error {
	author, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return fmt.Errorf("resolve merge-review status author: %w", err)
	}
	return reconcileMergeReviewStatusCommentAs(ctx, provider, repo, prNumber, author, body)
}

func reconcileMergeReviewStatusCommentAs(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, prNumber int, author, body string) error {
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

// electedLanderPass resolves an entirely-cross-PR-ordering `needs-changes`
// verdict into a genuine `pass` when this PR is its cluster's elected lander.
//
// WHAT ELECTION MEANS. Being elected does not mean "merge this regardless of
// review". It means "stop counting those siblings as blockers." And once that
// is said out loud, the verdict follows deterministically rather than by fiat:
// every finding was a pure ordering ask (allCrossPRBlocked — the PR is
// individually fine and merely waiting its turn), and this PR is the one whose
// turn it is. There is no defect left to fix, so there is nothing for
// `needs-changes` to describe. The decision is derived, not overridden.
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
func electedLanderPass(selectedNumber int, posted apiv1.Verdict) (bool, string) {
	if posted.Decision != apiv1.VerdictNeedsChanges {
		return false, ""
	}
	policy, policyName := resolveElectionPolicy(providerInput("electionPolicy", defaultElectionPolicy))
	if !electionDecision(posted.Findings, selectedNumber, policy) {
		return false, ""
	}
	return true, electedLanderPassRationale(selectedNumber, posted, policyName)
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
func mootFailReason(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr *providers.PullRequestSummary) (string, bool) {
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
func duplicateOfEarlierPR(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr *providers.PullRequestSummary) (string, bool) {
	mine := referencedIssueNumbers(pr.Body)
	if len(mine) == 0 {
		return "", false
	}
	mineSet := make(map[string]bool, len(mine))
	for _, id := range mine {
		mineSet[id] = true
	}
	others, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, HeadPrefix: "goobers/", SkipCheckState: true,
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
func closeMootPullRequest(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, selectedNumber int, current *providers.PullRequestSummary, verdict apiv1.Verdict, reason, resultFile string, stdout, stderr io.Writer) int {
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
	return writeApplyVerdictResultWithReason(path, selectedNumber, headSHA, baseSHA, decision, verdictAuthor, "", stderr)
}

func writeApplyVerdictResultWithReason(path string, selectedNumber int, headSHA, baseSHA, decision, verdictAuthor, reason string, stderr io.Writer) int {
	out := map[string]string{
		"selectedNumber":  strconv.Itoa(selectedNumber),
		"selectedHeadSha": headSHA,
		"selectedBaseSha": baseSHA,
		"decision":        decision,
		"verdictAuthor":   verdictAuthor,
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

func isTrustedMergeReviewStatusComment(commentAuthor, body, authenticatedAuthor string) bool {
	return isTrustedMergeReviewAuthor(commentAuthor, authenticatedAuthor) && isMergeReviewStatusComment(body)
}
