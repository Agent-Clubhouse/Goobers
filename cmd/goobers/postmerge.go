package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// closingKeywordPattern matches GitHub's own issue-closing keyword grammar
// (close/closes/closed, fix/fixes/fixed, resolve/resolves/resolved,
// case-insensitive) followed by a same-repo "#N" reference — the exact
// convention `goobers open-pr` writes ("Fixes #<issueID>", openpr.go).
var closingKeywordPattern = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)`)

// referenceKeywordPattern is the closing grammar widened to also match the
// non-closing "Implements #N" convention `goobers open-pr` writes in a
// structured PR body's Summary line (openprbody.go). It is deliberately
// broader than closingKeywordPattern: the open-PR eligibility backstop
// (#414/#980) wants to know whether ANY open PR already speaks for an issue,
// not only one whose body happens to carry a closing keyword. It still
// requires a directed keyword before the "#N" so a bare cross-reference
// mention ("see also #700") never over-excludes an unrelated issue.
var referenceKeywordPattern = regexp.MustCompile(`(?i)\b(?:implement(?:s|ed)?|close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)`)

// closingIssueNumbers extracts every distinct issue number a PR body
// references via GitHub's closing-keyword grammar, in first-seen order. Used
// by post-merge close-out (#355), which must only mark done the issues the PR
// actually closes — never a merely-referenced one — so it stays on the
// narrow closing grammar.
func closingIssueNumbers(body string) []string {
	return distinctIssueRefs(closingKeywordPattern, body)
}

// referencedIssueNumbers extracts every distinct issue number a PR body
// references via a directed keyword (implements/closes/fixes/resolves), in
// first-seen order. Used only by the backlog-query open-PR eligibility
// backstop (#980): a still-open PR that says "Implements #N" but omits a
// "Fixes #N" footer — an overridden/tutor body, or a future body format —
// must still exclude #N from re-selection, closing the gap that let #774 be
// implemented twice (#966/#969).
func referencedIssueNumbers(body string) []string {
	return distinctIssueRefs(referenceKeywordPattern, body)
}

// distinctIssueRefs returns the first submatch group of every match of
// pattern against body, de-duplicated in first-seen order.
func distinctIssueRefs(pattern *regexp.Regexp, body string) []string {
	matches := pattern.FindAllStringSubmatch(body, -1)
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

const needsRemediationLabel = "goobers:needs-remediation"

type siblingTriage struct {
	Reason           string
	OverlappingFiles []string
}

type postMergeRemediationHandoff struct {
	DisplacingPullNumber int      `json:"displacingPullNumber"`
	Reason               string   `json:"reason"`
	OverlappingFiles     []string `json:"overlappingFiles,omitempty"`
}

var postMergeRemediationPattern = regexp.MustCompile(`(?s)<!-- post-merge-remediation: (.*?) -->`)

func renderPostMergeRemediationHandoff(handoff postMergeRemediationHandoff) (string, error) {
	data, err := json.Marshal(handoff)
	if err != nil {
		return "", fmt.Errorf("marshal post-merge remediation handoff: %w", err)
	}

	prose := fmt.Sprintf(
		"**Post-merge remediation handoff**\n\nPull request #%d merged and displaced this PR (`%s`).",
		handoff.DisplacingPullNumber, handoff.Reason,
	)
	if len(handoff.OverlappingFiles) > 0 {
		prose += "\n\nOverlapping files:"
		for _, path := range handoff.OverlappingFiles {
			prose += fmt.Sprintf("\n- `%s`", path)
		}
	}
	return fmt.Sprintf("%s\n\n<!-- post-merge-remediation: %s -->", prose, data), nil
}

func parsePostMergeRemediationHandoff(body string) (postMergeRemediationHandoff, bool) {
	match := postMergeRemediationPattern.FindStringSubmatch(body)
	if match == nil {
		return postMergeRemediationHandoff{}, false
	}
	var handoff postMergeRemediationHandoff
	if err := json.Unmarshal([]byte(match[1]), &handoff); err != nil {
		return postMergeRemediationHandoff{}, false
	}
	return handoff, true
}

func persistPostMergeRemediationHandoff(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	prNumber int,
	author string,
	handoff postMergeRemediationHandoff,
) error {
	body, err := renderPostMergeRemediationHandoff(handoff)
	if err != nil {
		return err
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(prNumber))
	if err != nil {
		return fmt.Errorf("list existing handoff comments: %w", err)
	}
	for _, comment := range comments {
		existing, ok := parsePostMergeRemediationHandoff(comment.Body)
		if !isTrustedMergeReviewAuthor(comment.Author, author) || !ok ||
			existing.DisplacingPullNumber != handoff.DisplacingPullNumber {
			continue
		}
		if err := provider.UpdateComment(ctx, repo, comment.ID, body); err != nil {
			return fmt.Errorf("update existing handoff comment: %w", err)
		}
		return nil
	}
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         strconv.Itoa(prNumber),
		Comment:    body,
	}); err != nil {
		return fmt.Errorf("post handoff comment: %w", err)
	}
	return nil
}

// runPostMerge implements the `goobers post-merge` built-in stage kind
// (issue #361): the two actions that follow a successful merge-review merge.
//
//   - Post-merge fan-out (design doc §7 D7, triaged per issue #715): label
//     ONLY the other open PRs that actually need it — conflicted with the
//     base after the merge, or file-overlapping with the just-merged PR
//     (fanOutNeedsRemediation's own doc comment has the full triage
//     contract). A clean, disjoint sibling is left untouched; this is what
//     stopped feeding pr-remediation's O(N²) churn (#715: 481 remediation
//     runs across ~45 PRs, ≥94% no-op, before this fix).
//   - Close-out on merge (#355): the merged PR's body is parsed for its
//     closing-keyword issue reference(s) (the same "Fixes #N" convention
//     `goobers open-pr` writes), and each referenced issue is marked done.
//     The work isn't done until the merge, so this replaces
//     `implementation`'s old PR-open-time close (which now only sets
//     status=in-review — cmd/goobers/issuecloseout.go).
//
// Meant to run as the merge-review workflow's stage immediately after a
// successful `goobers merge-pr` (gated on its merged=true output). A PR that
// references no issue, or has no other open PRs to label, is a normal
// outcome (exit 0), not an error — not every merged PR closes a backlog item.
const postMergeHelp = "Usage: goobers post-merge [path]\n\n" +
	"Run the two actions that follow a successful merge: triage every\n" +
	"other open PR targeting the same base branch and label ONLY the\n" +
	"conflicted or file-overlapping ones goobers:needs-remediation, recording\n" +
	"the merged PR and overlapping paths on each affected PR (issue\n" +
	"#715 — a clean disjoint sibling is left untouched), and mark each\n" +
	"issue the merged PR's body references (Fixes/Closes/Resolves #N)\n" +
	"done. Declared input: pullNumber (required — the just-merged PR).\n" +
	"Exit codes: 0 = done (even if the PR body references no issue, or\n" +
	"there are no other open PRs — both are normal outcomes, not\n" +
	"errors), 1 = business error, 2 = usage/IO error.\n"

func runPostMerge(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("post-merge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "post-merge")
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
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	issuesToken, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newCachedGitHubProvider(root, prToken, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))
	issuesProvider := newCachedGitHubProvider(root, issuesToken, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))

	pullNumber := providerInput("pullNumber", "")
	if pullNumber == "" {
		pf(stderr, "error: pullNumber input is required\n")
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()

	var poll providers.PullRequestPollResult
	var pollErr error
	var postMergeErrs []error
	alreadyCompleted := false
	err = withPostMergeReconcileLock(root, func(ledgerPath string) error {
		ledger, err := readPostMergeReconcileLedger(ledgerPath)
		if err != nil {
			return err
		}
		if postMergeReconciliationCompleted(ledger, repo, pullNumber) {
			alreadyCompleted = true
			return nil
		}
		poll, pollErr = provider.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: repo, PullID: pullNumber})
		if pollErr != nil {
			return nil
		}
		postMergeErrs = performPostMerge(ctx, provider, issuesProvider, repo, root, pullNumber, poll, stdout, stderr)
		if len(postMergeErrs) > 0 {
			return nil
		}
		if completePostMergeReconciliation(&ledger, repo, pullNumber) {
			return writePostMergeReconcileLedger(ledgerPath, ledger)
		}
		return nil
	})
	if err != nil {
		pf(stderr, "error: record post-merge completion: %v\n", err)
		return 1
	}
	if pollErr != nil {
		return failProviderStage(stderr, "poll merged pull request", pollErr, "")
	}
	if alreadyCompleted {
		pf(stdout, "post-merge: pr #%s was already reconciled\n", pullNumber)
	}
	return 0
}

func performPostMerge(ctx context.Context, provider, issuesProvider *providers.GitHubProvider, repo providers.RepositoryRef, root, pullNumber string, poll providers.PullRequestPollResult, stdout, stderr io.Writer) []error {
	var errs []error
	labeled, skipped, labelErrs := fanOutNeedsRemediation(ctx, provider, repo, root, poll.Number, poll.BaseBranch, stderr)
	for _, lerr := range labelErrs {
		pf(stderr, "warning: %v\n", lerr)
	}
	errs = append(errs, labelErrs...)

	unparked, unparkErrs := unparkResolvedSiblings(ctx, provider, repo, poll.Number, poll.BaseBranch, stderr)
	for _, uerr := range unparkErrs {
		pf(stderr, "warning: %v\n", uerr)
	}
	errs = append(errs, unparkErrs...)

	unescalated, unescalateErrs := unparkSelfHealedEscalations(ctx, provider, repo, poll.Number, poll.BaseBranch, stderr)
	for _, eerr := range unescalateErrs {
		pf(stderr, "warning: %v\n", eerr)
	}
	errs = append(errs, unescalateErrs...)

	undemoted, undemoteErrs := unparkSelfHealedDemotions(ctx, provider, repo, poll.Number, poll.BaseBranch, stderr)
	for _, derr := range undemoteErrs {
		pf(stderr, "warning: %v\n", derr)
	}
	errs = append(errs, undemoteErrs...)

	closed, closeErrs := closeReferencedIssues(ctx, issuesProvider, repo, poll.Body, pullNumber)
	for _, cerr := range closeErrs {
		pf(stderr, "warning: %v\n", cerr)
	}
	errs = append(errs, closeErrs...)
	pf(stdout, "post-merge: labeled %d pr(s) %s (%d clean siblings left untouched), unparked %d blocked-on-sibling pr(s), un-escalated %d self-healed pr(s), un-demoted %d self-healed pr(s), closed %d issue(s)\n",
		len(labeled), needsRemediationLabel, len(skipped), len(unparked), len(unescalated), len(undemoted), len(closed))
	return errs
}

// unparkSelfHealedEscalations removes goobers:merge-escalated from any open PR
// that has self-healed since it was parked (#992/#836) — its own head/base SHA
// has moved past the escalation snapshot, so escalationStillBlocks now returns
// false. Until this, merge-escalated was never removed by any code path:
// escalationStillBlocks only made a self-healed PR re-selectable while the
// stale label physically remained (and, post-#986, kept it needlessly excluded
// from the open-PR cap). A merge advancing the base is one way a parked PR's
// recorded state goes stale, so post-merge is a natural sweep point. A genuine
// dead-end whose SHA has not moved (escalationStillBlocks fail-closed) keeps
// the label and its human handoff. Mirrors unparkResolvedSiblings' shape and
// best-effort error posture.
func unparkSelfHealedEscalations(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, mergedNumber int, base string, stderr io.Writer) (unparked []int, errs []error) {
	if base == "" {
		return nil, nil
	}
	others, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: providerBranchNamespace(), SkipCheckState: true,
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("list open pull requests targeting %s for merge-escalated unpark: %w", base, err))
		return nil, errs
	}
	for _, pr := range others {
		if pr.Number == mergedNumber {
			continue
		}
		if !hasAnyLabel(pr.Labels, []string{remediationEscalatedLabel}) {
			continue
		}
		stillBlocked, berr := escalationStillBlocks(ctx, provider, repo, pr)
		if berr != nil {
			errs = append(errs, fmt.Errorf("check merge-escalated state for pr #%d during unpark: %w", pr.Number, berr))
			continue
		}
		if stillBlocked {
			continue
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: strconv.Itoa(pr.Number), RemoveLabels: []string{remediationEscalatedLabel},
		}); err != nil {
			errs = append(errs, fmt.Errorf("clear %s from pr #%d: %w", remediationEscalatedLabel, pr.Number, err))
			continue
		}
		unparked = append(unparked, pr.Number)
	}
	return unparked, errs
}

// unparkSelfHealedDemotions removes goobers:merge-demoted from any open PR whose
// head has advanced past its demotion snapshot (#950) — demotionStillHolds now
// returns false, so it is no longer stuck at an unchanged head and should be
// allowed to win its own election again. A merge advancing the base commonly
// prompts the remediation pushes that move a demoted PR's head, so post-merge is
// a natural sweep point, exactly as it is for merge-escalated. A PR still stuck
// at the same head keeps the label. Mirrors unparkSelfHealedEscalations' shape
// and best-effort error posture.
func unparkSelfHealedDemotions(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, mergedNumber int, base string, stderr io.Writer) (healed []int, errs []error) {
	if base == "" {
		return nil, nil
	}
	others, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: "goobers/", SkipCheckState: true,
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("list open pull requests targeting %s for merge-demoted unpark: %w", base, err))
		return nil, errs
	}
	for _, pr := range others {
		if pr.Number == mergedNumber {
			continue
		}
		if !hasAnyLabel(pr.Labels, []string{mergeDemotedLabel}) {
			continue
		}
		stillDemoted, derr := demotionStillHolds(ctx, provider, repo, pr)
		if derr != nil {
			errs = append(errs, fmt.Errorf("check merge-demoted state for pr #%d during unpark: %w", pr.Number, derr))
			continue
		}
		if stillDemoted {
			continue
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: strconv.Itoa(pr.Number), RemoveLabels: []string{mergeDemotedLabel},
		}); err != nil {
			errs = append(errs, fmt.Errorf("clear %s from pr #%d: %w", mergeDemotedLabel, pr.Number, err))
			continue
		}
		healed = append(healed, pr.Number)
	}
	return healed, errs
}

// unparkResolvedSiblings clears goobers:blocked-on-sibling from every open
// sibling PR whose named blocker PRs are now ALL resolved after this merge
// (#748). The pull-based selection check (blockedOnSiblingStillBlocks) already
// makes such a PR selectable again on the next tick regardless of the label —
// this is the push-based half: it removes the now-stale label within one
// post-merge cycle so a PR parked on a blocker that already landed doesn't keep
// advertising a block that no longer exists. A PR with other, still-open
// blockers is left parked. Best-effort per PR, mirroring fanOutNeedsRemediation:
// a single failure is a warning, never fatal to the merge that already
// succeeded or to the other siblings.
func unparkResolvedSiblings(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, mergedNumber int, base string, stderr io.Writer) (unparked []int, errs []error) {
	if base == "" {
		return nil, nil
	}
	others, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: providerBranchNamespace(), SkipCheckState: true,
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("list open pull requests targeting %s for blocked-on-sibling unpark: %w", base, err))
		return nil, errs
	}
	for _, pr := range others {
		if pr.Number == mergedNumber {
			continue
		}
		if !hasAnyLabel(pr.Labels, []string{blockedOnSiblingLabel}) {
			continue
		}
		stillBlocked, berr := blockedOnSiblingStillBlocks(ctx, provider, repo, pr)
		if berr != nil {
			errs = append(errs, fmt.Errorf("check blocked-on-sibling state for pr #%d during unpark: %w", pr.Number, berr))
			continue
		}
		if stillBlocked {
			continue
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: strconv.Itoa(pr.Number), RemoveLabels: []string{blockedOnSiblingLabel},
		}); err != nil {
			errs = append(errs, fmt.Errorf("clear %s from pr #%d: %w", blockedOnSiblingLabel, pr.Number, err))
			continue
		}
		unparked = append(unparked, pr.Number)
	}
	return unparked, errs
}

// fanOutNeedsRemediation triages every OTHER open PR targeting base and
// labels needs-remediation only the ones that actually need it (issue #715):
// conflicted with the base after the merge, or file-overlapping with the
// just-merged PR (a semantic-collision risk the same guard the old blanket
// fan-out existed for — design doc §7 D7's stated rationale). A clean,
// disjoint sibling is left untouched: no label, so pr-remediation never
// selects it, force-pushes it, or restarts its CI — the O(N²) churn issue
// #715 measured (481 remediation runs across ~45 PRs, ≥94% no-op).
//
// Best-effort per PR: one failed label-apply is collected as a warning, not
// fatal to the others or to the merge that already succeeded. A per-sibling
// triage signal (mergeable check or files fetch) that itself fails is NOT
// silently treated as "clean" — it conservatively labels needs-remediation
// (the pre-#715 behavior for that one PR) rather than risk a false negative
// on an API hiccup; see triageSibling's own doc comment.
func fanOutNeedsRemediation(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, root string, mergedNumber int, base string, stderr io.Writer) (labeled, skipped []int, errs []error) {
	if base == "" {
		errs = append(errs, fmt.Errorf("merged PR has no recorded base branch, skipping fan-out"))
		return nil, nil, errs
	}

	// The merged PR's own files are the fixed side of every overlap check
	// below — fetched once, not per sibling. A failure here degrades every
	// sibling's overlap check to "no overlap detected" (mergedPaths stays
	// empty), not a fatal error: the mergeable check below still applies
	// independently, so triage degrades gracefully rather than failing shut.
	mergedPaths := map[string]bool{}
	if mergedFiles, ferr := provider.PullRequestFiles(ctx, repo, strconv.Itoa(mergedNumber)); ferr != nil {
		errs = append(errs, fmt.Errorf("list files for merged pr #%d (file-overlap triage degraded to mergeable-only for this run): %w", mergedNumber, ferr))
	} else {
		for _, f := range mergedFiles {
			mergedPaths[f.Path] = true
		}
	}

	// HeadPrefix scopes the fan-out to goober-authored siblings (G1's
	// goober-authored-repo assumption, matching pr-select/gather-sibling-
	// context's own default) — a human/other-agent PR sharing the same base
	// isn't pr-remediation's to touch. SkipCheckState: triage needs neither
	// field ListPullRequests would otherwise resolve per candidate.
	others, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: providerBranchNamespace(), SkipCheckState: true,
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("list open pull requests targeting %s: %w", base, err))
		return nil, nil, errs
	}

	// Opportunistic reuse of #523's sibling-context cache: if the SAME
	// merge-review run's earlier gather-sibling-context stage already fetched
	// a sibling's files this cycle (the common case — post-merge is the last
	// stage of the same run), triageSibling reuses them at zero extra cost
	// instead of re-fetching. A cold/corrupt/absent cache (any other
	// workflow, or a standalone invocation) degrades to nil here, which
	// triageSibling treats as an unconditional cache miss — never a failure.
	cached := loadSiblingCache(layoutFor(root).SchedulerDir(), stderr)

	var handoffAuthor string
	var handoffAuthorErr error
	handoffAuthorResolved := false
	for _, pr := range others {
		if pr.Number == mergedNumber {
			continue
		}
		triage, shouldLabel := triageSibling(ctx, provider, repo, pr, mergedPaths, cached, stderr)
		if !shouldLabel {
			skipped = append(skipped, pr.Number)
			continue
		}
		if !handoffAuthorResolved {
			handoffAuthor, handoffAuthorErr = provider.AuthenticatedLogin(ctx)
			handoffAuthorResolved = true
			if handoffAuthorErr != nil {
				errs = append(errs, fmt.Errorf("resolve post-merge handoff author: %w", handoffAuthorErr))
			}
		}
		if handoffAuthorErr != nil {
			continue
		}
		handoff := postMergeRemediationHandoff{
			DisplacingPullNumber: mergedNumber,
			Reason:               triage.Reason,
			OverlappingFiles:     triage.OverlappingFiles,
		}
		if err := persistPostMergeRemediationHandoff(ctx, provider, repo, pr.Number, handoffAuthor, handoff); err != nil {
			errs = append(errs, fmt.Errorf("persist remediation handoff on pr #%d (triage: %s): %w", pr.Number, triage.Reason, err))
			continue
		}
		if hasAnyLabel(pr.Labels, []string{needsRemediationLabel}) {
			continue
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: strconv.Itoa(pr.Number), AddLabels: []string{needsRemediationLabel},
		}); err != nil {
			errs = append(errs, fmt.Errorf("label pr #%d %s (triage: %s): %w", pr.Number, needsRemediationLabel, triage.Reason, err))
			continue
		}
		labeled = append(labeled, pr.Number)
	}
	return labeled, skipped, errs
}

// triageSibling decides whether pr needs the needs-remediation label,
// returning the diagnosis and every overlapping path. Mergeability still
// determines the reason first, but files are gathered too so a conflicted
// PR's durable handoff does not lose known overlap evidence:
//
//  1. Conflicted: GitHub's own computed mergeable=false. A nil (still
//     computing — normal right after a merge just changed the base)
//     is NOT treated as conflicted; that would false-positive-label a PR
//     that turns out clean once GitHub finishes computing it.
//  2. File-overlapping: pr's files intersect mergedPaths — the semantic-
//     collision guard design doc §7 D7 originally motivated the blanket
//     fan-out with, preserved here at sibling granularity instead of
//     instance-wide.
//
// A provider error on EITHER check is not swallowed as "clean" — it
// conservatively returns shouldLabel=true (matching pre-#715 behavior for
// that one PR), since a false negative here (silently skipping a PR that
// actually conflicts) risks exactly the "two textually-clean-looking PRs
// break main" failure mode post-merge main CI is the last backstop for, not
// the first.
func triageSibling(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, mergedPaths map[string]bool, cached map[string]siblingCacheEntry, stderr io.Writer) (siblingTriage, bool) {
	mergeable, merr := provider.PullRequestMergeable(ctx, repo, strconv.Itoa(pr.Number))
	if merr != nil {
		pf(stderr, "warning: check mergeable state for pr #%d: %v — conservatively labeling needs-remediation\n", pr.Number, merr)
	}

	files, ferr := siblingFilesForTriage(ctx, provider, repo, pr, cached)
	if ferr != nil {
		pf(stderr, "warning: list files for pr #%d: %v — conservatively labeling needs-remediation\n", pr.Number, ferr)
	}
	var overlappingFiles []string
	overlapSeen := make(map[string]bool)
	for _, f := range files {
		if mergedPaths[f] && !overlapSeen[f] {
			overlapSeen[f] = true
			overlappingFiles = append(overlappingFiles, f)
		}
	}
	sort.Strings(overlappingFiles)

	switch {
	case merr != nil:
		return siblingTriage{Reason: "mergeable-check-failed", OverlappingFiles: overlappingFiles}, true
	case mergeable != nil && !*mergeable:
		return siblingTriage{Reason: "conflicted", OverlappingFiles: overlappingFiles}, true
	case ferr != nil:
		return siblingTriage{Reason: "files-check-failed"}, true
	case len(overlappingFiles) > 0:
		return siblingTriage{
			Reason:           "file-overlap:" + strings.Join(overlappingFiles, ","),
			OverlappingFiles: overlappingFiles,
		}, true
	default:
		return siblingTriage{}, false
	}
}

// siblingFilesForTriage returns pr's touched files, reusing cached's entry
// when its recorded head SHA still matches pr's current one (siblingcache.go,
// issue #523) — cached is nil-safe (a plain map miss on a nil map behaves
// like an empty map). A cache miss or stale entry fetches fresh.
func siblingFilesForTriage(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary, cached map[string]siblingCacheEntry) ([]string, error) {
	if entry, ok := cached[strconv.Itoa(pr.Number)]; ok && entry.HeadSHA == pr.HeadSHA {
		return entry.Files, nil
	}
	files, err := provider.PullRequestFiles(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return paths, nil
}

// closeReferencedIssues marks every issue the merged PR's body references via
// GitHub's closing-keyword grammar (Fixes/Closes/Resolves #N) done. A PR
// referencing no issue is a normal outcome (not every PR closes a backlog
// item), not an error.
func closeReferencedIssues(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, body, pullNumber string) (closed []string, errs []error) {
	for _, issueID := range closingIssueNumbers(body) {
		if err := closeReferencedIssue(ctx, provider, repo, issueID, pullNumber); err != nil {
			errs = append(errs, fmt.Errorf("close issue #%s: %w", issueID, err))
			continue
		}
		closed = append(closed, issueID)
	}
	return closed, errs
}

func closeReferencedIssue(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, issueID, pullNumber string) error {
	item, err := provider.GetWorkItem(ctx, repo, issueID)
	if err != nil {
		return err
	}
	statusLabel := "goobers/status:" + string(providers.WorkItemStatusDone)
	if !strings.EqualFold(item.State, "closed") || !hasAnyLabel(item.Labels, []string{statusLabel}) {
		if _, err := provider.UpdateWorkItemStatus(ctx, providers.UpdateWorkItemStatusRequest{
			Repository: repo,
			ID:         issueID,
			Status:     providers.WorkItemStatusDone,
		}); err != nil {
			return err
		}
	}

	comment := fmt.Sprintf("Merged in pull request #%s.", pullNumber)
	comments, err := provider.ListComments(ctx, repo, issueID)
	if err != nil {
		return err
	}
	for _, existing := range comments {
		if existing.Body == comment {
			return nil
		}
	}
	_, err = provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         issueID,
		Comment:    comment,
	})
	return err
}
