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
	"regexp"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

var prReferencePattern = regexp.MustCompile(`(?i)\bPR\s*#\s*([0-9]+)\b`)

// hashReferencePattern additionally accepts the bare "#N" form ("with #597's
// runs list --json path") — live merge-review verdicts reference the selected
// PR this way inside finding messages, without the "PR" prefix
// prReferencePattern requires.
var hashReferencePattern = regexp.MustCompile(`#\s*([0-9]+)\b`)

type remediationPriority uint8

const (
	remediationPriorityNone remediationPriority = iota
	remediationPriorityBehindBase
	remediationPriorityFailingCI
	remediationPriorityNeedsRemediation
)

// prThreadComment is one comment on the PR thread — human/other-agent review
// feedback, or a prior merge-review verdict comment — surfaced as context
// for whatever addresses the PR next (design doc §5: pr-remediation reads
// "the Verdict artifact, PR-thread comments, and behind/conflict state").
type prThreadComment struct {
	Author    string `json:"author,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// runGatherPRContext implements `goobers gather-pr-context` (issue #362):
// pr-remediation's entrypoint, replacing implementation's query-backlog head
// (design doc §5 — "the one genuinely new executor entrypoint"). Selects one
// open, goober-authored PR labeled needs-remediation, reporting failing CI, or
// behind its base, checks out ITS branch into this stage's worktree (replacing
// whatever branch the runner's worktree provisioning defaulted to —
// pr-remediation re-enters on an EXISTING PR, it does not open a new one), and
// loads the merge-review Verdict + PR-thread comments + whether the base has
// advanced since this PR branched, as context for the stages that follow
// (#363's rebase + finding-driven routing).
func runGatherPRContext(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gather-pr-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers gather-pr-context [path]\n\n"+
			"Select one open, goober-authored PR labeled goobers:needs-remediation\n"+
			"or reporting failing CI, falling back to a PR behind its base only when\n"+
			"neither stronger signal is present. Check out its branch into this\n"+
			"stage's worktree and load the latest merge-review verdict + PR-thread\n"+
			"comments + whether the base has advanced since this PR branched, writing\n"+
			"them to the declared result file. [path] is the instance root (matching\n"+
			"pr-select/apply-verdict), defaulting to GOOBERS_INSTANCE_ROOT; git\n"+
			"operations run against the stage's actual worktree (the process's\n"+
			"current directory), not path — same split push-branch already relies\n"+
			"on. Exit codes: 0 = context gathered (or no-work if no PR is eligible),\n"+
			"1 = business error, 2 = usage/IO error.\n")
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
	// issues:write and repo:push are both used below (ListComments hits the
	// issues API; the checkout is a git operation) — both checked explicitly
	// before any call is made, matching #360/#361's capability-absent-refuses-
	// first contract. In V0 all three resolve to the identical repo credential
	// (runnerwiring.go's credentialedCapabilities), so only prToken is
	// actually needed to construct the provider.
	if _, err := providerToken(capability.GitHubIssuesWrite); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(prToken)

	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", "goobers/")

	ctx := context.Background()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, "pr-context.json")
	}

	// #716's core fix, applied upstream of #596's tier selection: a PR
	// carrying goobers:merge-escalated only stays excluded from EVERY tier
	// (needs-remediation, failing-CI, and the behind-base fallback alike)
	// while its live head/base SHA still match the snapshot recorded at
	// escalation time — escalationStillBlocks is self-heal-aware, unlike a
	// static label check, so a PR that self-healed (new commits, or a
	// sibling merge advancing its base) is filtered back IN here and
	// reaches whichever tier its other signals qualify it for. Filtering
	// here, before tiering, means an excluded top-tier PR correctly falls
	// through to a lower tier's candidates rather than forcing a no-work
	// cycle when a perfectly eligible lower-tier PR exists.
	var nonBlocked []providers.PullRequestSummary
	for _, pr := range prs {
		blocked, err := escalationStillBlocks(ctx, provider, repo, pr)
		if err != nil {
			return failProviderStage(stderr, fmt.Sprintf("check escalation state for PR #%d", pr.Number), err, "pr-context.json")
		}
		if blocked {
			continue
		}
		// #748: also exclude a PR parked goobers:blocked-on-sibling whose
		// named blockers are not all resolved yet — same blocker-aware
		// self-heal pr-select applies, so gather-pr-context (pr-remediation's
		// selection) never spends a cycle on a PR waiting purely on a sibling.
		sibBlocked, err := blockedOnSiblingStillBlocks(ctx, provider, repo, pr)
		if err != nil {
			return failProviderStage(stderr, fmt.Sprintf("check blocked-on-sibling state for PR #%d", pr.Number), err, "pr-context.json")
		}
		if sibBlocked {
			continue
		}
		nonBlocked = append(nonBlocked, pr)
	}

	fetchedBases := make(map[string]bool)
	candidates, _, err := selectRemediationCandidates(nonBlocked, func(pr providers.PullRequestSummary) (bool, error) {
		if !fetchedBases[pr.Base] {
			if _, err := fetchExistingBranch(".", pr.Base, pushToken); err != nil {
				return false, fmt.Errorf("fetch base branch %q: %w", pr.Base, err)
			}
			fetchedBases[pr.Base] = true
		}
		headSHA, err := fetchExistingBranch(".", pr.Head, pushToken)
		if err != nil {
			return false, fmt.Errorf("fetch PR #%d branch %q: %w", pr.Number, pr.Head, err)
		}
		return isCommitBehindBase(".", pr.BaseSHA, headSHA)
	})
	if err != nil {
		pf(stderr, "error: determine remediation eligibility: %v\n", err)
		return 1
	}
	if len(candidates) == 0 {
		return writeNoWorkResult(stdout, stderr, "no PR needs remediation this cycle")
	}

	claimed, err := claimEligiblePullRequest(root, candidates)
	if err != nil {
		pf(stderr, "error: claim eligible PR: %v\n", err)
		return 1
	}
	if claimed == nil {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}
	selected := *claimed

	if _, err := checkoutExistingBranch(".", selected.Head, pushToken); err != nil {
		pf(stderr, "error: checkout PR #%d's branch %q: %v\n", selected.Number, selected.Head, err)
		return 1
	}

	behind, err := isBehindBase(".", selected.BaseSHA)
	if err != nil {
		pf(stderr, "error: check base ancestry for PR #%d: %v\n", selected.Number, err)
		return 1
	}

	rawComments, err := provider.ListComments(ctx, repo, strconv.Itoa(selected.Number))
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("list comments on PR #%d", selected.Number), err, "pr-context.json")
	}
	// Latest comment carrying an embedded payload wins (a PR can accumulate
	// several merge-review cycles' worth of comments; only the most recent
	// verdict is still actionable).
	var verdict *apiv1.Verdict
	for i := len(rawComments) - 1; i >= 0; i-- {
		if v, ok := parseVerdictComment(rawComments[i].Body); ok {
			verdict = &v
			break
		}
	}

	// Digest short-circuit (#716 design item 2): escalationStillBlocks above
	// only excludes a PR whose LIVE goobers:merge-escalated label matches its
	// recorded snapshot exactly — it does not fire once that label is gone
	// (self-healed via a new head/base, or cleared by a human) OR once the
	// PR's base has moved just enough to change its recorded base SHA. Either
	// way, this PR was selected because ITS SELECTION criteria say "go", but
	// if the actual `git diff base...HEAD` content is still byte-identical to
	// what was recorded at the last escalation, running rebase-pr/remediation
	// again cannot make progress — bail as a clean no-work tick instead of
	// spending a cycle (worktree provision, checkout, potential agentic
	// work) reproducing the exact escalation remediation-checkpoint already
	// recorded.
	if remState, _, ok := latestRemediationState(rawComments); ok && remState.Escalated && remState.LastDiffDigest != "" {
		digest, derr := diffDigest(".", selected.BaseSHA)
		if derr != nil {
			pf(stderr, "error: compute diff digest for PR #%d: %v\n", selected.Number, derr)
			return 1
		}
		if digest == remState.LastDiffDigest {
			return writeNoWorkResult(stdout, stderr, fmt.Sprintf(
				"PR #%d's diff (digest %s) is unchanged since its last recorded escalation — no progress possible this cycle",
				selected.Number, digest,
			))
		}
	}

	comments := make([]prThreadComment, 0, len(rawComments))
	for _, c := range rawComments {
		createdAt := ""
		if c.CreatedAt != nil {
			createdAt = c.CreatedAt.Format(time.RFC3339)
		}
		comments = append(comments, prThreadComment{Author: c.Author, Body: c.Body, CreatedAt: createdAt})
	}

	// hasSubstantiveFindings is a plain "true"/"false" STRING, not a native
	// bool: internal/executor's InputResultFile convention only threads
	// string-valued top-level result-file keys through Task.InputsFrom into
	// a downstream stage's actual GOOBERS_INPUT_* env var (a bool/object
	// value survives into the run's Outputs map fine, but is silently
	// dropped at that later step) — #363's rebase-pr is the first consumer
	// and needs this to arrive intact. selectedNumber is stringified for the
	// exact same reason (matching pr-select's own strconv.Itoa convention).
	hasSubstantiveFindings := "false"
	if verdictHasSubstantiveFindingForPR(verdict, selected.Number) {
		hasSubstantiveFindings = "true"
	}
	hasFailingCI := strconv.FormatBool(selected.CheckState == providers.CheckStateFailing)

	resultFile := providerInput("resultFile", "pr-context.json")
	data, err := json.MarshalIndent(map[string]interface{}{
		"selectedNumber": strconv.Itoa(selected.Number),
		"head":           selected.Head,
		// The runner's well-known branch-rebinding output (issue #392,
		// runner.WorkspaceBranchOutput): every stage AFTER this one gets its
		// worktree provisioned on the PR's own head branch instead of a fresh
		// branch cut from base. That is what lets pr-remediation reuse
		// implementation's implement/review/local-ci chain verbatim — those
		// stages, and the agentic reviewer gate, have no way to re-checkout
		// anything for themselves the way this stage and rebase-pr do, and the
		// reviewer's runner-computed `git diff base...HEAD` evidence is only
		// the PR's real diff if its worktree is on the PR's branch.
		//
		// Same value as "head" deliberately: the rebinding is a distinct
		// CONTRACT with the runner, not a second name for a field a stage
		// happens to also thread to rebase-pr via inputsFrom, and renaming or
		// dropping "head" must not silently un-wire the chain.
		"workspaceBranch":        selected.Head,
		"base":                   selected.Base,
		"headSha":                selected.HeadSHA,
		"baseSha":                selected.BaseSHA,
		"isBehindBase":           behind,
		"hasSubstantiveFindings": hasSubstantiveFindings,
		"hasFailingCI":           hasFailingCI,
		"verdict":                verdict,
		"comments":               comments,
	}, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal pr context: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "gathered context for PR #%d (%s): behind=%v, %d comment(s)\n", selected.Number, selected.Head, behind, len(comments))
	return 0
}

// remediationPriorityFor classifies a single PR's remediation urgency,
// independent of its peers — needs-remediation outranks failing CI, and
// neither implies anything about whether the PR is merely behind its base
// (that's a fallback tier, checked only when nothing clears these two: see
// selectRemediationCandidates). Escalation exclusion is NOT this function's
// concern: runGatherPRContext's self-heal-aware escalationStillBlocks
// (#716) pre-filters prs before selectRemediationCandidates ever sees them,
// so every pr reaching here has already cleared that check — a static
// re-check of the label here would incorrectly re-exclude a PR that just
// self-healed (still labeled merge-escalated, but its head/base moved past
// the recorded escalation snapshot).
func remediationPriorityFor(pr providers.PullRequestSummary) remediationPriority {
	switch {
	case hasAnyLabel(pr.Labels, []string{needsRemediationLabel}):
		return remediationPriorityNeedsRemediation
	case pr.CheckState == providers.CheckStateFailing:
		return remediationPriorityFailingCI
	}
	return remediationPriorityNone
}

// selectRemediationCandidates returns every open PR at the single strongest
// remediation-priority tier present (needs-remediation, else failing CI),
// so claimEligiblePullRequest can still try each in ascending-number order
// for exactly-once selection across concurrent runs — returning only ONE
// pre-picked PR here would mean a concurrent run's claim on that PR strands
// every other eligible PR for a full cycle, regressing the pre-#596-fallback
// contract of offering the WHOLE eligible set to the claim ledger.
//
// Only when nothing clears either tier does a PR merely behind its base
// become eligible at all (#596's fix — previously such a PR was only
// rebased as a side effect of something else flagging it first). Checking
// "behind base" requires fetching each candidate's branches, so behindBase
// is only invoked when nothing stronger exists — the priority tiers above
// are decidable from the PR summary alone, no fetch required.
func selectRemediationCandidates(prs []providers.PullRequestSummary, behindBase func(providers.PullRequestSummary) (bool, error)) ([]providers.PullRequestSummary, remediationPriority, error) {
	var candidates []providers.PullRequestSummary
	best := remediationPriorityNone
	for _, pr := range prs {
		switch p := remediationPriorityFor(pr); {
		case p == remediationPriorityNone:
			continue
		case p > best:
			best = p
			candidates = []providers.PullRequestSummary{pr}
		case p == best:
			candidates = append(candidates, pr)
		}
	}
	if best != remediationPriorityNone {
		return candidates, best, nil
	}

	for _, pr := range prs {
		// Same rationale as remediationPriorityFor: escalation exclusion
		// already happened upstream (self-heal-aware), so no re-check here.
		behind, err := behindBase(pr)
		if err != nil {
			return nil, remediationPriorityNone, err
		}
		if behind {
			candidates = append(candidates, pr)
		}
	}
	if len(candidates) > 0 {
		best = remediationPriorityBehindBase
	}
	return candidates, best, nil
}

// verdictHasSubstantiveFindingForPR reports whether verdict carries a
// substantive finding attributable to the selected PR itself. Attribution
// rules, in order:
//
//   - A Location with no "PR #N" reference is file/line-scoped within the
//     selected PR's own diff — counts (#525's retain-file-scoped rule).
//   - A Location referencing the selected PR counts.
//   - A Location referencing only sibling PRs is NOT automatically a
//     sibling's own issue (#608): merge-review's cross-PR-conflict findings
//     point Location at the sibling ("PR #598") while the Message states
//     what the SELECTED PR is blocked on ("Reconcile ... with #597's runs
//     list --json row shape"). If the Message references the selected PR —
//     "PR #597" or the bare "#597" live verdicts actually use — the finding
//     is about the selected PR's own mergeability and counts. Dropping
//     these made rebase-pr report needsAgent:false on every cycle of a
//     genuinely deadlocked PR, violating its "a clean rebase never
//     suppresses a known substantive finding" contract.
//   - Otherwise the finding describes a sibling's own issue and is excluded
//     (#525: a plain-rebase PR must not be misrouted into agentic
//     remediation by findings that aren't about it).
func verdictHasSubstantiveFindingForPR(verdict *apiv1.Verdict, prNumber int) bool {
	if verdict == nil {
		return false
	}
	target := strconv.Itoa(prNumber)
	for _, finding := range verdict.Findings {
		if finding.Class != apiv1.FindingSubstantive {
			continue
		}
		locationRefs := prReferencePattern.FindAllStringSubmatch(finding.Location, -1)
		if len(locationRefs) == 0 {
			return true
		}
		if referencesTarget(locationRefs, target) {
			return true
		}
		if referencesTarget(hashReferencePattern.FindAllStringSubmatch(finding.Message, -1), target) {
			return true
		}
	}
	return false
}

// referencesTarget reports whether any captured PR-number reference equals
// target (matches come from prReferencePattern or hashReferencePattern, both
// of which capture the number as the first submatch).
func referencesTarget(matches [][]string, target string) bool {
	for _, match := range matches {
		if match[1] == target {
			return true
		}
	}
	return false
}

// checkoutExistingBranch fetches branch from origin and checks it out at
// dir, replacing whatever the runner's worktree provisioning checked out by
// default (a fresh run-scoped branch off base — irrelevant here, since
// pr-remediation re-enters on an EXISTING PR's branch rather than opening a
// new one). EVERY stage in pr-remediation.yaml gets its own fresh worktree
// (see internal/runner's per-stage-attempt worktree provisioning), so this
// is not a one-time setup step — rebase-pr (#363) calls it again for
// exactly this reason, not out of redundancy. Authenticated via gitAuthEnv,
// shared with push-branch's gitPushBranch (#237): never a URL-embedded
// credential, never persisted to disk.
//
// Returns the branch's remote SHA at the moment of THIS fetch — rebase-pr's
// eventual force-with-lease push must compare against this exact value (the
// state this stage started from), never a value re-resolved right before
// pushing: re-resolving immediately before the push would make the lease
// tautological (it would always match whatever just landed), silently
// defeating the "don't clobber a concurrent push" guarantee force-with-lease
// exists for.
func checkoutExistingBranch(dir, branch, token string) (fetchedSHA string, err error) {
	fetchedSHA, err = fetchExistingBranch(dir, branch, token)
	if err != nil {
		return "", err
	}
	checkout := exec.Command("git", "checkout", "-B", branch, "FETCH_HEAD")
	checkout.Dir = dir
	if out, err := checkout.CombinedOutput(); err != nil {
		return "", fmt.Errorf("checkout %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	return fetchedSHA, nil
}

// fetchExistingBranch fetches branch from origin into dir and returns its
// remote SHA, without checking it out — used both by checkoutExistingBranch
// (which checks out on top) and by selectRemediationCandidates' behind-base
// probe (which only needs the SHA to compare ancestry, and must not disturb
// dir's currently-checked-out branch while probing OTHER PRs' candidacy).
func fetchExistingBranch(dir, branch, token string) (string, error) {
	url, err := originURL(dir)
	if err != nil {
		return "", err
	}
	env := gitAuthEnv(token)
	fetch := exec.Command("git", "fetch", url, "refs/heads/"+branch)
	fetch.Dir = dir
	fetch.Env = env
	if out, err := fetch.CombinedOutput(); err != nil {
		return "", fmt.Errorf("fetch %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	rev := exec.Command("git", "rev-parse", "FETCH_HEAD")
	rev.Dir = dir
	out, err := rev.Output()
	if err != nil {
		return "", fmt.Errorf("resolve fetched SHA for %s: %w", branch, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isBehindBase reports whether baseSHA is NOT an ancestor of the checked-out
// HEAD at dir — i.e. the base branch has advanced since this PR branched, so
// a rebase (issue #363) will be needed. This only detects staleness; it
// never attempts the rebase itself (design doc §5 D3: routing is
// finding-driven, never rebase-driven — that decision belongs to the stage
// after this one).
func isBehindBase(dir, baseSHA string) (bool, error) {
	return isCommitBehindBase(dir, baseSHA, "HEAD")
}

// isCommitBehindBase is isBehindBase generalized to an arbitrary headSHA
// (rather than always the dir's checked-out HEAD) — selectRemediationCandidates'
// behind-base probe needs to test candidate PRs it hasn't checked out.
func isCommitBehindBase(dir, baseSHA, headSHA string) (bool, error) {
	if baseSHA == "" {
		return false, fmt.Errorf("PR has no recorded base SHA")
	}
	if headSHA == "" {
		return false, fmt.Errorf("PR has no recorded head SHA")
	}
	cmd := exec.Command("git", "merge-base", "--is-ancestor", baseSHA, headSHA)
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", baseSHA, headSHA, err)
}
