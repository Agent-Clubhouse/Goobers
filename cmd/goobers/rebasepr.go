package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/mergeresolve"
	"github.com/goobers/goobers/providers"
)

// runRebasePR implements `goobers rebase-pr` (issue #363): pr-remediation's
// rebase-first, finding-driven routing (design doc §5 D3). Routing is never
// rebase-driven: a clean rebase never suppresses a known substantive
// finding, and a rebase conflict is itself substantive.
//
//	rebase result | finding or failing CI? | action
//	clean         | no                     | force-with-lease push, clear label, done
//	clean         | yes                    | needs the agentic chain (not yet wired, see pr-remediation.yaml)
//	unsafe conflict | either               | needs the agentic chain (the conflict IS substantive)
//
// Re-checks out the PR's own branch first (checkoutExistingBranch, shared
// with gather-pr-context): this stage gets its OWN fresh worktree — see
// checkoutExistingBranch's doc comment — so it cannot assume gather-pr-
// context's checkout survived. A conflict confined to the generated portal
// bundle is resolved by rebuilding that bundle, and a conflict made only of
// distinct entries inserted into the same existing line-oriented list is
// resolved mechanically; all other conflicts retain the agentic path.
const rebasePRHelp = "Usage: goobers rebase-pr [path]\n\n" +
	"Check out the selected PR's branch, attempt a rebase onto its base\n" +
	"(force-with-lease is mandatory for the eventual push — never a bare\n" +
	"push), and route on the result: a clean rebase with no detected,\n" +
	"policy-allowed cause force-pushes and clears goobers:needs-remediation;\n" +
	"a detected cause the declared policy allows needs the agentic remediation\n" +
	"chain, reported via the needsAgent output for the workflow to route on; a\n" +
	"detected cause the policy excludes is left untouched for a human\n" +
	"(policyExcluded/policyExcludedReason outputs — #941/PRR-6). Requires\n" +
	"selectedNumber/head/base (Task.InputsFrom gather-pr-context's own\n" +
	"outputs) and hasSubstantiveFindings/hasFailingCI.\n\n" +
	"remediate (input, default \"conflict,substantive,failing-ci,behind-base,\n" +
	"sibling-overlap,human-comment\") is a comma-separated policy naming which\n" +
	"detected causes are allowed to trigger remediation; the shipped default is\n" +
	"fully liberal. behind-base is accepted vocabulary but cannot fire yet (no\n" +
	"detection reaches this stage's decision today). human-comment fires when a\n" +
	"genuinely new human comment postdates the watermark recorded in the sticky\n" +
	"remediation-state comment; detection runs only when the declared policy\n" +
	"names human-comment, so an old pinned policy is unaffected.\n\n" +
	"Exit codes: 0 = routed, 1 = business error, 2 = usage/IO error.\n"

func runRebasePR(args []string, stdout, stderr io.Writer) int {
	ctx, cancel := providerCommandContext()
	defer cancel()

	fs := newCLIFlagSet("rebase-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "rebase-pr")
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

	resultFile := providerInput("resultFile", "rebase-result.json")
	selectedNumber := providerInput("selectedNumber", "")
	head := providerInput("head", "")
	base := providerInput("base", providerBaseBranch())
	attemptedHeadSHA := ""
	rebaseBaseSHA := ""
	hasSubstantiveFindings := providerInput("hasSubstantiveFindings", "false") == "true"
	hasFailingCI := providerInput("hasFailingCI", "false") == "true"
	hasSiblingOverlap := providerInput("hasSiblingOverlap", "false") == "true"
	remediate := providerInput("remediate", defaultRemediatePolicy)
	conflict := false
	var conflictLocations []rebaseConflictLocation
	// Pre-checkout this evaluation only ever feeds the error path (fail), which
	// has not yet had a chance to detect a new human comment — pass false.
	policy := evaluateRemediatePolicy(remediate, conflict, hasSubstantiveFindings, hasFailingCI, hasSiblingOverlap, false)
	fail := func(err error) int {
		return failRebasePR(
			stderr, resultFile, selectedNumber, head, attemptedHeadSHA, rebaseBaseSHA,
			conflict, conflictLocations, policy, err,
		)
	}
	if selectedNumber == "" || head == "" {
		return fail(errors.New("selectedNumber and head are required (inputsFrom gather-pr-context's own outputs)"))
	}
	selectedPRNumber, err := strconv.Atoi(selectedNumber)
	if err != nil {
		return fail(fmt.Errorf("invalid selectedNumber %q: %w", selectedNumber, err))
	}

	repo, err := providerRepo(root)
	if err != nil {
		return fail(err)
	}
	if repo.Provider == providers.ProviderADO {
		return runRebasePRADO(root, repo, stdout, stderr)
	}
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		return fail(err)
	}
	issuesToken, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		return fail(err)
	}
	provider, err := remediationStageProvider(root, repo, issuesToken, false)
	if err != nil {
		return fail(err)
	}
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return fail(err)
	}
	handoffProvider, err := remediationStageProvider(root, repo, prToken, false)
	if err != nil {
		return fail(err)
	}

	attemptedHeadSHA, err = checkoutExistingBranch(".", head, pushToken)
	if err != nil {
		return fail(fmt.Errorf("checkout PR #%s's branch %q: %w", selectedNumber, head, err))
	}

	siblingHandoffs, hasSiblingHandoff, err := trustedSiblingOverlapHandoffs(
		ctx, handoffProvider, repo, selectedNumber, attemptedHeadSHA,
	)
	hasSiblingOverlap = hasSiblingOverlap || hasSiblingHandoff
	if hasSiblingHandoff && hasSubstantiveFindings && siblingHandoffs.verdict != nil {
		hasSubstantiveFindings = verdictHasIndependentSubstantiveFindingForPR(
			siblingHandoffs.verdict,
			selectedPRNumber,
			siblingHandoffs.displacingPullNumbers,
			resolveMinSeverity(stderr),
		)
	}
	// Detect a genuinely new human comment ONLY when the declared policy names
	// human-comment (#941/PRR-6 gate). Skipping the provider calls entirely for
	// an old pinned policy that omits the cause is what keeps such a workflow
	// byte-for-byte unaffected — the parking-regression hazard (design §0/§6).
	hasNewHumanComment := false
	if remediatePolicyAllows(remediate, remediateCauseHumanComment) {
		comments, err := handoffProvider.ListComments(ctx, repo, selectedNumber)
		if err != nil {
			return fail(fmt.Errorf("list comments on PR #%s: %w", selectedNumber, err))
		}
		botLogin, err := handoffProvider.AuthenticatedLogin(ctx)
		if err != nil {
			return fail(fmt.Errorf("resolve authenticated login for PR #%s: %w", selectedNumber, err))
		}
		hasNewHumanComment = hasNewHumanCommentSince(comments, botLogin)
	}
	policy = evaluateRemediatePolicy(remediate, conflict, hasSubstantiveFindings, hasFailingCI, hasSiblingOverlap, hasNewHumanComment)
	if err != nil {
		return fail(fmt.Errorf("load post-merge remediation handoff for PR #%s: %w", selectedNumber, err))
	}

	conflict, conflictLocations, rebaseBaseSHA, err = attemptRebase(".", base, pushToken)
	policy = evaluateRemediatePolicy(remediate, conflict, hasSubstantiveFindings, hasFailingCI, hasSiblingOverlap, hasNewHumanComment)
	if err != nil {
		return fail(fmt.Errorf("rebase PR #%s onto %q: %w", selectedNumber, base, err))
	}

	if !policy.needsAgent {
		// Nothing detected at all — the liberal-default behavior this
		// reproduces exactly regardless of the declared policy.
		if err := forcePushWithLease(".", head, attemptedHeadSHA, pushToken); err != nil {
			return fail(fmt.Errorf("force-push rebased PR #%s branch %q: %w", selectedNumber, head, err))
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: selectedNumber, RemoveLabels: []string{needsRemediationLabel},
		}); err != nil {
			return fail(fmt.Errorf("clear %s from PR #%s: %w", needsRemediationLabel, selectedNumber, err))
		}
		if err := writeRebaseResult(resultFile, selectedNumber, head, false, false, policyResult{}, nil, attemptedHeadSHA, rebaseBaseSHA); err != nil {
			return fail(err)
		}
		pf(stdout, "PR #%s: clean rebase onto %s, no substantive finding — force-pushed and cleared %s\n", selectedNumber, base, needsRemediationLabel)
		return 0
	}

	if policy.policyExcluded {
		// A cause WAS detected, but the declared `remediate` policy excludes
		// every detected cause (#941/PRR-6). Force-pushing here would
		// silently drop the excluded finding forever (clearing the label
		// with no record); instead this cycle is left untouched for a human,
		// and remediation-checkpoint (reading policyExcluded/
		// policyExcludedReason below) escalates immediately rather than
		// spending a repass budget on a cause the policy declined to touch.
		if err := writeRebaseResult(resultFile, selectedNumber, head, conflict, true, policy.policyResult, conflictLocations, attemptedHeadSHA, rebaseBaseSHA); err != nil {
			return fail(err)
		}
		pf(stdout, "PR #%s: %s\n", selectedNumber, policy.excludedReason)
		return 0
	}

	// At least one detected cause is allowed by the declared policy — same
	// force-push-to-retrigger-CI-only behavior as before when the rebase
	// itself is clean (conflict=false, substantive=false; only failing-ci or
	// human-comment fired), since that push does not touch or hide the firing
	// cause. A human-comment-only cycle likewise just force-pushes the clean
	// rebase (safe: it neither rewrites content nor drops a finding) and defers
	// to the checkpoint for the agentic response to the comment.
	if !conflict && !hasSubstantiveFindings && !hasSiblingOverlap {
		if err := forcePushWithLease(".", head, attemptedHeadSHA, pushToken); err != nil {
			return fail(fmt.Errorf("force-push rebased PR #%s branch %q: %w", selectedNumber, head, err))
		}
	}

	if err := writeRebaseResult(resultFile, selectedNumber, head, conflict, true, policy.policyResult, conflictLocations, attemptedHeadSHA, rebaseBaseSHA); err != nil {
		return fail(err)
	}
	pf(stdout, "PR #%s needs agentic remediation (conflict=%v, substantiveFindings=%v, failingCI=%v) — routing to remediation checkpoint\n", selectedNumber, conflict, hasSubstantiveFindings, hasFailingCI)
	return 0
}

// runRebasePRADO runs the rebase-pr stage on Azure DevOps. The git core —
// checkout, fetch/rebase (with the portal-bundle and adjacent-line
// auto-resolution), and the mandatory force-with-lease — is provider-neutral and
// shared verbatim with the GitHub path (checkoutExistingBranch, attemptRebase,
// forcePushWithLease, evaluateRemediatePolicy, writeRebaseResult, failRebasePR).
// Only three things differ on ADO, all reached because *ADOProvider cannot
// satisfy remediationProvider (remediation-wiring-plan §0.1/§3.2):
//
//   - The clean-rebase label clear routes to the native PR-label DELETE
//     (RemovePullRequestLabel, §2.6) instead of UpdateWorkItem(ID: PR#), which on
//     ADO would mutate the unrelated work item that shares the PR's numeric id
//     (the wrong-object hazard, §0.5).
//   - The trusted-sibling-overlap handoff scan is GitHub-only remediation
//     machinery (it reads PR issue comments and uses identity semantics ADO
//     lacks and takes a remediationProvider *ADOProvider cannot satisfy); it never
//     runs, so hasSiblingOverlap stays whatever gather-pr-context reported
//     (always false on ADO).
//   - human-comment detection never runs: the ADO remediate policy drops that
//     cause (§3.2), so no PR-comment scan is needed and — exactly as on GitHub —
//     an old pinned policy is byte-for-byte unaffected.
//
// The provider is built from config-sourced ADO auth via the shared stage factory
// (no github:* token is resolved); only the provider-neutral repo:push
// credential feeds the git operations.
func runRebasePRADO(root string, repo providers.RepositoryRef, stdout, stderr io.Writer) int {
	ctx, cancel := providerCommandContext()
	defer cancel()

	resultFile := providerInput("resultFile", "rebase-result.json")
	selectedNumber := providerInput("selectedNumber", "")
	head := providerInput("head", "")
	base := providerInput("base", providerBaseBranch())
	attemptedHeadSHA := ""
	rebaseBaseSHA := ""
	hasSubstantiveFindings := providerInput("hasSubstantiveFindings", "false") == "true"
	hasFailingCI := providerInput("hasFailingCI", "false") == "true"
	hasSiblingOverlap := providerInput("hasSiblingOverlap", "false") == "true"
	remediate := providerInput("remediate", defaultRemediatePolicy)
	conflict := false
	var conflictLocations []rebaseConflictLocation
	// human-comment is never detected on ADO (see the doc comment) — pass false.
	policy := evaluateRemediatePolicy(remediate, conflict, hasSubstantiveFindings, hasFailingCI, hasSiblingOverlap, false)
	fail := func(err error) int {
		return failRebasePR(
			stderr, resultFile, selectedNumber, head, attemptedHeadSHA, rebaseBaseSHA,
			conflict, conflictLocations, policy, err,
		)
	}
	if selectedNumber == "" || head == "" {
		return fail(errors.New("selectedNumber and head are required (inputsFrom gather-pr-context's own outputs)"))
	}
	if _, err := strconv.Atoi(selectedNumber); err != nil {
		return fail(fmt.Errorf("invalid selectedNumber %q: %w", selectedNumber, err))
	}

	provider, err := newProviderForStageAs[*providers.ADOProvider](root, repo, true)
	if err != nil {
		return fail(err)
	}
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		return fail(err)
	}

	attemptedHeadSHA, err = checkoutExistingBranch(".", head, pushToken)
	if err != nil {
		return fail(fmt.Errorf("checkout PR #%s's branch %q: %w", selectedNumber, head, err))
	}

	conflict, conflictLocations, rebaseBaseSHA, err = attemptRebase(".", base, pushToken)
	policy = evaluateRemediatePolicy(remediate, conflict, hasSubstantiveFindings, hasFailingCI, hasSiblingOverlap, false)
	if err != nil {
		return fail(fmt.Errorf("rebase PR #%s onto %q: %w", selectedNumber, base, err))
	}

	if !policy.needsAgent {
		// Nothing detected at all — the liberal-default behavior this reproduces
		// exactly regardless of the declared policy.
		if err := forcePushWithLease(".", head, attemptedHeadSHA, pushToken); err != nil {
			return fail(fmt.Errorf("force-push rebased PR #%s branch %q: %w", selectedNumber, head, err))
		}
		// The re-entry trigger: clear the marker via the native PR-label DELETE,
		// NEVER UpdateWorkItem(ID: PR#) — that is the wrong-object hazard on ADO.
		if err := provider.RemovePullRequestLabel(ctx, repo, selectedNumber, needsRemediationLabel); err != nil {
			return fail(fmt.Errorf("clear %s from PR #%s: %w", needsRemediationLabel, selectedNumber, err))
		}
		if err := writeRebaseResult(resultFile, selectedNumber, head, false, false, policyResult{}, nil, attemptedHeadSHA, rebaseBaseSHA); err != nil {
			return fail(err)
		}
		pf(stdout, "PR #%s: clean rebase onto %s, no substantive finding — force-pushed and cleared %s\n", selectedNumber, base, needsRemediationLabel)
		return 0
	}

	if policy.policyExcluded {
		// A cause WAS detected, but the declared policy excludes every detected
		// cause (#941/PRR-6) — leave this cycle untouched for a human rather than
		// force-pushing or dropping the finding.
		if err := writeRebaseResult(resultFile, selectedNumber, head, conflict, true, policy.policyResult, conflictLocations, attemptedHeadSHA, rebaseBaseSHA); err != nil {
			return fail(err)
		}
		pf(stdout, "PR #%s: %s\n", selectedNumber, policy.excludedReason)
		return 0
	}

	// At least one detected cause is allowed by the declared policy. Same
	// force-push-to-retrigger-CI-only behavior as the GitHub path when the rebase
	// itself is clean (only failing-ci fired): that push touches neither the
	// firing cause nor a finding.
	if !conflict && !hasSubstantiveFindings && !hasSiblingOverlap {
		if err := forcePushWithLease(".", head, attemptedHeadSHA, pushToken); err != nil {
			return fail(fmt.Errorf("force-push rebased PR #%s branch %q: %w", selectedNumber, head, err))
		}
	}

	if err := writeRebaseResult(resultFile, selectedNumber, head, conflict, true, policy.policyResult, conflictLocations, attemptedHeadSHA, rebaseBaseSHA); err != nil {
		return fail(err)
	}
	pf(stdout, "PR #%s needs agentic remediation (conflict=%v, substantiveFindings=%v, failingCI=%v) — routing to remediation checkpoint\n", selectedNumber, conflict, hasSubstantiveFindings, hasFailingCI)
	return 0
}

// Remediation policy causes (#941/PRR-6): the vocabulary a workflow's
// `remediate` input names. defaultRemediatePolicy is the shipped, fully
// liberal default — every cause pr-remediation can detect fires the agentic
// chain, reproducing today's behavior exactly.
const (
	remediateCauseConflict       = "conflict"
	remediateCauseSubstantive    = "substantive"
	remediateCauseFailingCI      = "failing-ci"
	remediateCauseBehindBase     = "behind-base"
	remediateCauseSiblingOverlap = "sibling-overlap"
	remediateCauseHumanComment   = "human-comment"
)

// defaultRemediatePolicy lists every cause name. behind-base is accepted
// policy vocabulary but cannot fire yet: its detection (gather-pr-context's
// isBehindBase) is a native bool in the versioned RemediationBrief schema,
// and only string-valued result-file keys survive Task.InputsFrom into a
// downstream stage's input (see hasSubstantiveFindings/hasFailingCI's own
// comment in gatherprcontext.go) — wiring it here needs a brief schema
// version bump, tracked as a follow-up. The name is still accepted so a
// workflow author can declare the eventual full policy now without a
// validation error, per the design's "policy visible in shipped YAML"
// requirement.
//
// human-comment fires only when a genuinely new human comment postdates the
// watermark recorded in the sticky remediation-state comment
// (remediationState.LastSeenCommentAt) — a bot comment, the sticky state
// comment itself, and a comment at or before the watermark never trigger.
// Detection is deliberately gated on this cause appearing in the declared
// `remediate` policy (remediatePolicyAllows), so an old pinned-policy YAML
// that omits it is byte-for-byte unaffected and a clean green PR that merely
// received a comment is never parked.
const defaultRemediatePolicy = remediateCauseConflict + "," + remediateCauseSubstantive + "," +
	remediateCauseFailingCI + "," + remediateCauseBehindBase + "," + remediateCauseSiblingOverlap + "," +
	remediateCauseHumanComment

// policyResult carries the declared-policy outcome rebase-pr's result file
// forwards to remediation-checkpoint.
type policyResult struct {
	excluded bool
	reason   string
	causes   []remediationCause
}

// remediatePolicyOutcome is evaluateRemediatePolicy's return value.
type remediatePolicyOutcome struct {
	needsAgent     bool
	policyExcluded bool
	excludedReason string
	policyResult   policyResult
}

// remediatePolicyAllows reports whether the declared `remediate` policy names
// cause. Detection of the human-comment cause is gated on this (see
// runRebasePR): a workflow whose pinned policy omits human-comment must not
// even list PR comments, so a clean green PR that merely received a comment is
// never parked under an old policy (the parking-regression hazard, design §0).
func remediatePolicyAllows(remediate, cause string) bool {
	for _, declared := range splitLabelList(remediate) {
		if declared == cause {
			return true
		}
	}
	return false
}

// hasNewHumanCommentSince reports whether comments contains a genuinely new
// human comment that should retrigger remediation, using the watermark the
// last checkpoint recorded in the sticky remediation-state comment
// (remediationState.LastSeenCommentAt).
//
// Scope limits: issue-level comments only — inline review threads are a
// different provider API and out of scope here; edited comments do not
// retrigger, since providers.Comment carries no UpdatedAt to compare against.
//
// A comment triggers iff ALL hold: it has a CreatedAt; its author is not a Bot
// (GitHub sets User.Type; Gitea leaves it empty, which passes); it is not the
// authenticated bot itself (isTrustedMergeReviewAuthor); and its body carries
// no machine payload (neither a remediation-state nor a verdict comment —
// defensive, since those are the bot's own).
//
// Watermark comparison, from latestRemediationState(comments):
//   - state found but LastSeenCommentAt == "": pre-upgrade state, fail closed —
//     the whole PR returns false so a fleet upgrade never retriggers en masse.
//   - state found with an unparsable LastSeenCommentAt: false (fail closed).
//   - state found with a valid watermark: a comment triggers iff its CreatedAt
//     is strictly After the watermark.
//   - no state ever recorded: fail open — every qualifying human comment counts
//     (bounded by the per-cause budget downstream).
func hasNewHumanCommentSince(comments []providers.Comment, botLogin string) bool {
	state, _, found := latestRemediationState(comments)
	var watermark time.Time
	haveWatermark := false
	if found {
		if state.LastSeenCommentAt == "" {
			return false
		}
		parsed, err := time.Parse(time.RFC3339, state.LastSeenCommentAt)
		if err != nil {
			return false
		}
		watermark = parsed
		haveWatermark = true
	}
	for _, c := range comments {
		if c.CreatedAt == nil {
			continue
		}
		if strings.EqualFold(c.AuthorType, "Bot") {
			continue
		}
		if isTrustedMergeReviewAuthor(c.Author, botLogin) {
			continue
		}
		if _, ok := parseRemediationStateComment(c.Body); ok {
			continue
		}
		if _, ok := parseVerdictComment(c.Body); ok {
			continue
		}
		if haveWatermark && !c.CreatedAt.After(watermark) {
			continue
		}
		return true
	}
	return false
}

// evaluateRemediatePolicy applies the declared `remediate` policy (#941/
// PRR-6) to this cycle's detected causes.
func evaluateRemediatePolicy(remediate string, conflict, substantive, failingCI, siblingOverlap, humanComment bool) remediatePolicyOutcome {
	allowed := make(map[string]bool)
	for _, cause := range splitLabelList(remediate) {
		allowed[cause] = true
	}

	detected := []struct {
		name    remediationCause
		present bool
	}{
		{remediationCauseConflict, conflict},
		{remediationCauseSubstantive, substantive},
		{remediationCauseFailingCI, failingCI},
		{remediationCauseSiblingOverlap, siblingOverlap},
		{remediationCauseHumanComment, humanComment},
	}

	var firing []remediationCause
	var excluded []string
	for _, cause := range detected {
		if !cause.present {
			continue
		}
		if allowed[string(cause.name)] {
			firing = append(firing, cause.name)
		} else {
			excluded = append(excluded, string(cause.name))
		}
	}

	outcome := remediatePolicyOutcome{needsAgent: len(firing) > 0 || len(excluded) > 0}
	outcome.policyResult.causes = firing
	if len(firing) == 0 && len(excluded) > 0 {
		outcome.policyExcluded = true
		outcome.excludedReason = fmt.Sprintf(
			"remediation policy %q excludes the only detected cause(s) (%s) — leaving it for a human rather than force-pushing or silently rewriting",
			remediate, strings.Join(excluded, ", "),
		)
		outcome.policyResult.excluded = true
		outcome.policyResult.reason = outcome.excludedReason
	}
	return outcome
}

func failRebasePR(
	stderr io.Writer,
	resultFile, selectedNumber, head, attemptedHeadSHA, rebaseBaseSHA string,
	conflict bool,
	conflictLocations []rebaseConflictLocation,
	policy remediatePolicyOutcome,
	err error,
) int {
	pf(stderr, "error: %v\n", err)
	code, retryable, extra := classifyProviderError(err)
	locationsJSON := "[]"
	if len(conflictLocations) > 0 {
		if data, marshalErr := json.Marshal(conflictLocations); marshalErr == nil {
			locationsJSON = string(data)
		}
	}
	payload := map[string]interface{}{
		"selectedNumber":              selectedNumber,
		"head":                        head,
		"needsAgent":                  "true",
		"conflict":                    strconv.FormatBool(conflict),
		"conflictLocations":           locationsJSON,
		"attemptedHeadSha":            attemptedHeadSHA,
		"rebaseBaseSha":               rebaseBaseSHA,
		"remediationCauses":           formatRemediationCauses(policy.policyResult.causes),
		"policyExcluded":              strconv.FormatBool(policy.policyExcluded),
		"policyExcludedReason":        policy.excludedReason,
		executor.OutputErrorCode:      code,
		executor.OutputErrorMessage:   err.Error(),
		executor.OutputErrorRetryable: retryable,
	}
	for key, value := range extra {
		payload[key] = value
	}
	if writeErr := writeProviderStageResult(resultFile, payload); writeErr != nil {
		pf(stderr, "warning: write typed error result %s: %v\n", resultFile, writeErr)
	}
	return 1
}

// writeRebaseResult echoes selectedNumber/head forward alongside this
// stage's own needsAgent/conflict outcome — Task.InputsFrom resolves
// against the immediately preceding TASK's own Outputs (a gate never
// updates that chain; the gate this stage feeds is proof: apply-verdict's
// own doc comment establishes the same convention), so remediation-
// checkpoint (after rebase-gate) can only read selectedNumber/head if THIS
// stage re-emits them, exactly like gather-sibling-context re-emits
// pr-select's selectedNumber for apply-verdict two hops later.
func writeRebaseResult(
	resultFile, selectedNumber, head string,
	conflict, needsAgent bool,
	policy policyResult,
	conflictLocations []rebaseConflictLocation,
	attemptedHeadSHA string,
	rebaseBaseSHA string,
) error {
	locationsJSON, err := json.Marshal(conflictLocations)
	if err != nil {
		return fmt.Errorf("marshal rebase conflict locations: %w", err)
	}
	data, err := json.Marshal(map[string]string{
		"selectedNumber":       selectedNumber,
		"head":                 head,
		"needsAgent":           strconv.FormatBool(needsAgent),
		"conflict":             strconv.FormatBool(conflict),
		"conflictLocations":    string(locationsJSON),
		"attemptedHeadSha":     attemptedHeadSHA,
		"rebaseBaseSha":        rebaseBaseSHA,
		"remediationCauses":    formatRemediationCauses(policy.causes),
		"policyExcluded":       strconv.FormatBool(policy.excluded),
		"policyExcludedReason": policy.reason,
	})
	if err != nil {
		return fmt.Errorf("marshal rebase result: %w", err)
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", resultFile, err)
	}
	return nil
}

func formatRemediationCauses(causes []remediationCause) string {
	values := make([]string, len(causes))
	for i, cause := range causes {
		values[i] = string(cause)
	}
	return strings.Join(values, ",")
}

type trustedSiblingHandoffs struct {
	displacingPullNumbers []int
	verdict               *apiv1.Verdict
}

func trustedSiblingOverlapHandoffs(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	selectedNumber string,
	targetHeadSHA string,
) (trustedSiblingHandoffs, bool, error) {
	comments, err := provider.ListComments(ctx, repo, selectedNumber)
	if err != nil {
		return trustedSiblingHandoffs{}, false, err
	}
	if !hasPotentialSiblingOverlapHandoff(comments, targetHeadSHA) {
		return trustedSiblingHandoffs{}, false, nil
	}
	author, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return trustedSiblingHandoffs{}, false, err
	}
	type matchingHandoff struct {
		comment providers.Comment
		handoff postMergeRemediationHandoff
	}
	var matches []matchingHandoff
	seenPullNumbers := make(map[int]bool)
	found := trustedSiblingHandoffs{verdict: gatherPRVerdict(comments, author)}
	for i := len(comments) - 1; i >= 0; i-- {
		if !isTrustedMergeReviewAuthor(comments[i].Author, author) {
			continue
		}
		handoff, ok := parsePostMergeRemediationHandoff(comments[i].Body)
		if !ok || !isSiblingOverlapHandoff(handoff) {
			continue
		}
		if handoff.TargetHeadSHA != "" && handoff.TargetHeadSHA != targetHeadSHA {
			continue
		}
		matches = append(matches, matchingHandoff{
			comment: comments[i],
			handoff: handoff,
		})
		if !seenPullNumbers[handoff.DisplacingPullNumber] {
			seenPullNumbers[handoff.DisplacingPullNumber] = true
			found.displacingPullNumbers = append(found.displacingPullNumbers, handoff.DisplacingPullNumber)
		}
	}
	if len(matches) == 0 {
		return trustedSiblingHandoffs{}, false, nil
	}
	for _, match := range matches {
		if match.handoff.Version != 0 {
			continue
		}
		match.handoff.Version = postMergeRemediationHandoffVersion
		match.handoff.TargetHeadSHA = targetHeadSHA
		body, err := renderPostMergeRemediationHandoff(match.handoff)
		if err != nil {
			return found, true, err
		}
		if err := provider.UpdateComment(ctx, repo, match.comment.ID, body); err != nil {
			return found, true, fmt.Errorf("migrate legacy post-merge remediation handoff: %w", err)
		}
	}
	return found, true, nil
}

func hasPotentialSiblingOverlapHandoff(comments []providers.Comment, targetHeadSHA string) bool {
	for i := len(comments) - 1; i >= 0; i-- {
		handoff, ok := parsePostMergeRemediationHandoff(comments[i].Body)
		if !ok || !isSiblingOverlapHandoff(handoff) {
			continue
		}
		if handoff.TargetHeadSHA == targetHeadSHA ||
			(handoff.Version == 0 && handoff.TargetHeadSHA == "") {
			return true
		}
	}
	return false
}

func isSiblingOverlapHandoff(handoff postMergeRemediationHandoff) bool {
	return len(handoff.OverlappingFiles) > 0 || strings.HasPrefix(handoff.Reason, "file-overlap:")
}

// attemptRebase mechanically resolves conflicts confined to the generated
// portal bundle, plus the narrow case where both sides added one distinct
// entry to the same existing line-oriented list at an unambiguous ancestor
// position. Every other conflict is inspected for structural-collision
// evidence, aborted cleanly, and reported for the existing agentic path.
func attemptRebase(dir, base, token string) (conflict bool, locations []rebaseConflictLocation, rebaseBaseSHA string, err error) {
	url, err := originURL(dir)
	if err != nil {
		return false, nil, "", err
	}
	auth := gitAuthEnv(token)
	fetch := exec.Command("git", "fetch", url, "refs/heads/"+base)
	fetch.Dir = dir
	fetch.Env = auth
	if out, err := fetch.CombinedOutput(); err != nil {
		return false, nil, "", fmt.Errorf("fetch base %s: %w: %s", base, err, strings.TrimSpace(string(out)))
	}
	baseRev := exec.Command("git", "rev-parse", "FETCH_HEAD")
	baseRev.Dir = dir
	baseOut, err := baseRev.Output()
	if err != nil {
		return false, nil, "", gitOutputError("git rev-parse FETCH_HEAD", err)
	}
	rebaseBaseSHA = strings.TrimSpace(string(baseOut))

	rebase := exec.Command("git", rebaseFetchHeadArgs(dir)...)
	rebase.Dir = dir
	out, rerr := rebase.CombinedOutput()
	if rerr == nil {
		return false, nil, rebaseBaseSHA, nil
	}

	observedConflict := false
	for {
		status, resolveErr := resolvePortalDistConflicts(dir)
		if status == rebaseConflictAbsent && resolveErr == nil {
			status, resolveErr = resolveAdjacentLineConflicts(dir)
		}
		observedConflict = observedConflict || status != rebaseConflictAbsent
		if resolveErr != nil {
			return observedConflict, locations, rebaseBaseSHA, abortRebaseAfterError(dir, resolveErr)
		}
		if status != rebaseConflictResolved {
			if status == rebaseConflictAbsent {
				rebaseErr := fmt.Errorf("git rebase FETCH_HEAD: %w: %s", rerr, strings.TrimSpace(string(out)))
				return observedConflict, locations, rebaseBaseSHA, abortRebaseAfterError(dir, rebaseErr)
			}
			var inspectErr error
			locations, inspectErr = currentRebaseConflictLocations(dir)
			if inspectErr != nil {
				return observedConflict, locations, rebaseBaseSHA, abortRebaseAfterError(dir, fmt.Errorf("inspect rebase conflict: %w", inspectErr))
			}
			if err := abortRebase(dir); err != nil {
				return observedConflict, locations, rebaseBaseSHA, fmt.Errorf("git rebase FETCH_HEAD: %w: %s; %w", rerr, strings.TrimSpace(string(out)), err)
			}
			return true, locations, rebaseBaseSHA, nil
		}

		cont := exec.Command("git", "rebase", "--continue")
		cont.Dir = dir
		cont.Env = append(os.Environ(), "GIT_EDITOR=true")
		continueOut, continueErr := cont.CombinedOutput()
		if continueErr == nil {
			return false, nil, rebaseBaseSHA, nil
		}

		nextStatus, statusErr := unmergedConflictStatus(dir)
		observedConflict = observedConflict || nextStatus != rebaseConflictAbsent
		if statusErr != nil {
			return observedConflict, locations, rebaseBaseSHA, abortRebaseAfterError(dir, statusErr)
		}
		if nextStatus == rebaseConflictAbsent {
			rebaseErr := fmt.Errorf("git rebase --continue: %w: %s", continueErr, strings.TrimSpace(string(continueOut)))
			return observedConflict, locations, rebaseBaseSHA, abortRebaseAfterError(dir, rebaseErr)
		}
		out, rerr = continueOut, continueErr
	}
}

func rebaseFetchHeadArgs(dir string) []string {
	help := exec.Command("git", "rebase", "-h")
	help.Dir = dir
	out, _ := help.CombinedOutput()
	if strings.Contains(string(out), "reapply-cherry-picks") {
		return []string{"rebase", "--no-reapply-cherry-picks", "FETCH_HEAD"}
	}
	return []string{"rebase", "FETCH_HEAD"}
}

// The conflict vocabulary and the adjacent-line resolution rules are shared
// with the implementation workflow's pre-CI base synchronization
// (internal/worktree's syncBase merge, #3096) — one implementation of what is
// provably safe to resolve mechanically, not two that can drift apart.
type rebaseConflictStatus = mergeresolve.Status

const (
	rebaseConflictAbsent   = mergeresolve.StatusAbsent
	rebaseConflictUnsafe   = mergeresolve.StatusUnsafe
	rebaseConflictResolved = mergeresolve.StatusResolved
)

const portalDistPath = "cmd/goobers/portal-dist"

// execGit adapts this command's plain exec-based git invocation to the shared
// resolver's runner seam.
func execGit(dir string) mergeresolve.Git {
	return func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return nil, gitOutputError("git "+strings.Join(args, " "), err)
		}
		return out, nil
	}
}

func resolvePortalDistConflicts(dir string) (rebaseConflictStatus, error) {
	files, err := unmergedConflictFiles(dir)
	if err != nil {
		return rebaseConflictAbsent, err
	}
	if len(files) == 0 {
		return rebaseConflictAbsent, nil
	}
	for _, file := range files {
		if !strings.HasPrefix(file.Path, portalDistPath+"/") {
			return rebaseConflictAbsent, nil
		}
	}

	build := exec.Command("make", "portal-build")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		return rebaseConflictUnsafe, fmt.Errorf("regenerate portal bundle: %w: %s", err, strings.TrimSpace(string(out)))
	}
	add := exec.Command("git", "--literal-pathspecs", "add", "-A", "--", portalDistPath)
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		return rebaseConflictUnsafe, fmt.Errorf("stage regenerated portal bundle: %w: %s", err, strings.TrimSpace(string(out)))
	}
	remaining, err := unmergedConflictFiles(dir)
	if err != nil {
		return rebaseConflictUnsafe, err
	}
	if len(remaining) != 0 {
		return rebaseConflictUnsafe, fmt.Errorf("stage regenerated portal bundle: %d paths remain unmerged", len(remaining))
	}
	return rebaseConflictResolved, nil
}

func resolveAdjacentLineConflicts(dir string) (rebaseConflictStatus, error) {
	return mergeresolve.ResolveAdjacentLineConflicts(dir, execGit(dir))
}

func unmergedConflictStatus(dir string) (rebaseConflictStatus, error) {
	files, err := unmergedConflictFiles(dir)
	if err != nil {
		return rebaseConflictAbsent, err
	}
	if len(files) == 0 {
		return rebaseConflictAbsent, nil
	}
	return rebaseConflictUnsafe, nil
}

func unmergedConflictFiles(dir string) ([]mergeresolve.File, error) {
	return mergeresolve.UnmergedFiles(execGit(dir))
}

func hasStandardTextMergeAttributes(dir, path string) (bool, error) {
	return mergeresolve.HasStandardTextMergeAttributes(execGit(dir), path)
}

func mergeAdjacentLineInsertions(path string, ancestor, upstream, incoming []byte) ([]byte, bool) {
	return mergeresolve.MergeAdjacentLineInsertions(path, ancestor, upstream, incoming)
}

func abortRebase(dir string) error {
	abort := exec.Command("git", "rebase", "--abort")
	abort.Dir = dir
	if out, err := abort.CombinedOutput(); err != nil {
		return fmt.Errorf("abort rebase: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func abortRebaseAfterError(dir string, cause error) error {
	if err := abortRebase(dir); err != nil {
		return fmt.Errorf("%w; %w", cause, err)
	}
	return cause
}

// forcePushWithLease pushes branch to origin with an explicit
// --force-with-lease=<branch>:<expectedSHA> (design doc §5: "mandatory —
// even in a goober-authored repo a human may push to a branch; the lease
// makes Goobers lose gracefully and re-select next tick rather than clobber
// the push"), authenticated via gitAuthEnv, shared with push-branch's plain
// gitPushBranch (#237) — never a URL-embedded or persisted credential. A
// rebase rewrites history, so push-branch's own non-force push (correct for
// implementation's linear-commit flow) would always be rejected here.
//
// expectedSHA MUST be the branch's remote tip captured at checkout time
// (checkoutExistingBranch's own return value) — NOT re-resolved here right
// before pushing. Re-resolving immediately before the push would make the
// lease tautological (it would always match whatever just landed on the
// remote, silently defeating the "refuse if something pushed since I
// started" guarantee this function exists for — caught by
// TestRebasePRForceWithLeaseRefusesOnConcurrentPush). Plain
// --force-with-lease (no explicit expected value) isn't an option either:
// this binary fetches by resolved URL, not the named "origin" remote
// (originURL's own doc comment explains why — a mirrored remote can't take
// an explicit refspec), so no refs/remotes/origin/<branch> tracking ref is
// ever updated for the bare flag to compare against, which misreports every
// push as "stale info" regardless of whether the remote actually moved.
func forcePushWithLease(dir, branch, expectedSHA, token string) error {
	url, err := originURL(dir)
	if err != nil {
		return err
	}
	cmd := exec.Command("git", "push", "--force-with-lease="+branch+":"+expectedSHA, url, branch+":"+branch)
	cmd.Dir = dir
	cmd.Env = gitAuthEnv(token)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
