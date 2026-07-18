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
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// blockedOnSiblingLabel marks a PR that's correct in isolation but must wait
// behind a named sibling (#747) — see verdictLabel's doc comment.
const blockedOnSiblingLabel = "goobers:blocked-on-sibling"

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

// runApplyVerdict implements `goobers apply-verdict` (issue #359): reads the
// holistic review gate's Verdict back from this run's own journal (the gate
// already records it as an artifact via internal/gate's recordVerdict — no
// new plumbing), re-checks its SHA pin against the PR's CURRENT head/base
// before acting (design doc §6 D6: a verdict computed against a state that
// no longer exists is void, not actionable), then publishes the verdict as a
// SHA-pinned native GitHub review. Non-pass verdicts retain the existing
// prose-comment + decision-label handoff consumed by pr-remediation.
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
			"re-check its SHA pin against the PR's current head/base, and — if\n"+
			"still valid — post the verdict as a native GitHub review. Non-pass\n"+
			"verdicts also retain the remediation label + PR-comment handoff. A\n"+
			"stale SHA pin voids the verdict: no comment, no label, exit 0 (this\n"+
			"cycle's work is simply moot, not an error — merge-review re-reviews\n"+
			"next tick). Requires selectedNumber (Task.InputsFrom pr-select's\n"+
			"number output). Exit codes: 0 = applied (or voided), 1 = business\n"+
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
	ctx := context.Background()
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
		return writeApplyVerdictResult(resultFile, selectedNumber, "", "", "moot", stderr)
	}

	// D6: the verdict is void if the PR has moved since it was computed —
	// either new commits landed (headSha changed) or the base advanced
	// (baseSha changed). Acting on a stale verdict would label/comment
	// against a diff that no longer exists.
	if verdict.HeadSHA != "" && verdict.HeadSHA != current.HeadSHA {
		pf(stdout, "verdict void: PR #%d's head moved (%s -> %s) since review — skipping, will re-review next cycle\n", selectedNumber, verdict.HeadSHA, current.HeadSHA)
		return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, "moot", stderr)
	}
	if verdict.BaseSHA != "" && verdict.BaseSHA != current.BaseSHA {
		pf(stdout, "verdict void: PR #%d's base moved (%s -> %s) since review — skipping, will re-review next cycle\n", selectedNumber, verdict.BaseSHA, current.BaseSHA)
		return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, "moot", stderr)
	}

	posted := *verdict
	if posted.Digest == "" {
		posted.Digest = providerInput("reviewDigest", "")
	}
	if posted.SourceRunID == "" {
		posted.SourceRunID = runID
	}

	comment := renderVerdictComment(posted)
	label := verdictLabel(posted.Decision, posted.Findings)
	if label == blockedOnSiblingLabel {
		state := blockedOnSiblingState{
			Blockers:   unionBlockingPRs(posted.Findings),
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
		return failProviderStage(stderr, fmt.Sprintf("submit native review for PR #%d", selectedNumber), err, resultFile)
	}

	if posted.Decision == apiv1.VerdictPass {
		pf(stdout, "approved PR #%d at %s\n", selectedNumber, current.HeadSHA)
		return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(posted.Decision), stderr)
	}

	// Publish the native review first. If the legacy handoff below fails, the
	// absence of an exclusion label leaves the PR eligible for a later
	// merge-review run instead of stranding it without a platform verdict.
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         strconv.Itoa(selectedNumber),
		AddLabels:  []string{label},
		Comment:    comment,
	}); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("apply verdict to PR #%d", selectedNumber), err, resultFile)
	}

	pf(stdout, "applied %s to PR #%d (%s)\n", label, selectedNumber, verdict.Decision)
	return writeApplyVerdictResult(resultFile, selectedNumber, current.HeadSHA, current.BaseSHA, string(posted.Decision), stderr)
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

func writeApplyVerdictResult(path string, selectedNumber int, headSHA, baseSHA, decision string, stderr io.Writer) int {
	data, err := json.Marshal(map[string]string{
		"selectedNumber":  strconv.Itoa(selectedNumber),
		"selectedHeadSha": headSHA,
		"selectedBaseSha": baseSHA,
		"decision":        decision,
	})
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
// message. It also embeds the SAME Verdict as a machine-readable payload
// (verdictJSONComment) in an HTML comment appended to the end — invisible
// when GitHub renders the comment, but readable by `gather-pr-context`
// (issue #362), which runs in a different workflow's run and so has no
// journal/runID relationship to this run's own artifact. This keeps the
// prose and the machine payload as ONE posted comment (still a single
// source of truth) rather than growing a second, driftable channel.
func renderVerdictComment(v apiv1.Verdict) string {
	s := fmt.Sprintf("**merge-review verdict: %s**\n\n%s", v.Decision, v.Summary)
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
