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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

var prReferencePattern = regexp.MustCompile(`(?i)\bPR\s*#\s*([0-9]+)\b`)

// hashReferencePattern additionally accepts the bare "#N" form ("with #597's
// runs list --json path") — live merge-review verdicts reference the selected
// PR this way inside finding messages, without the "PR" prefix
// prReferencePattern requires.
var hashReferencePattern = regexp.MustCompile(`#\s*([0-9]+)\b`)

const remediationBriefResultFile = "remediation-brief.json"

type remediationPriority uint8

const (
	remediationPriorityNone remediationPriority = iota
	remediationPriorityBehindBase
	remediationPriorityFailingCI
	remediationPriorityNeedsRemediation
)

const gatherPRContextHelp = "Usage: goobers gather-pr-context [path]\n\n" +
	"Select one open, goober-authored PR labeled goobers:needs-remediation\n" +
	"or reporting failing CI, falling back to a PR behind its base only when\n" +
	"neither stronger signal is present. Check out its branch into this\n" +
	"stage's worktree and load the latest merge-review verdict + PR-thread\n" +
	"comments + whether the base has advanced since this PR branched, writing\n" +
	"the versioned remediation-brief artifact to the declared result file.\n" +
	"[path] is the instance root (matching\n" +
	"pr-select/apply-verdict), defaulting to GOOBERS_INSTANCE_ROOT; git\n" +
	"operations run against the stage's actual worktree (the process's\n" +
	"current directory), not path — same split push-branch already relies\n" +
	"on. Exit codes: 0 = context gathered (or no-work if no PR is eligible),\n" +
	"1 = business error, 2 = usage/IO error.\n"

// runGatherPRContext implements `goobers gather-pr-context` (issue #362):
// pr-remediation's entrypoint, replacing implementation's query-backlog head
// (design doc §5 — "the one genuinely new executor entrypoint"). Selects one
// open, goober-authored PR labeled needs-remediation, reporting failing CI, or
// crowned by live parked dependents and behind its base. It checks out ITS
// branch into this stage's worktree (replacing whatever branch the runner's
// worktree provisioning defaulted to — pr-remediation re-enters on an EXISTING
// PR, it does not open a new one), and loads the merge-review Verdict +
// PR-thread comments + whether the base has advanced since this PR branched, as
// context for the stages that follow (#363's rebase + finding-driven routing).
func runGatherPRContext(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("gather-pr-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "gather-pr-context")
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
	// The remediation loop selects flagged PRs oldest-first (FIFO). Validate
	// any configured remediationAlgorithm override up front — provider-agnostic,
	// so it runs before the ADO branch — so an unrecognized value warns rather
	// than silently selecting fifo anyway.
	validateRemediationAlgorithm(stderr)
	// Azure DevOps pr-remediation epic: gather-pr-context's ADO branch selects a
	// needs-remediation PR from the label tier, rebinds its branch, and recovers
	// the merge-review verdict from the PR THREAD (ADO has no PR-comment
	// transport). It never resolves a github:* token and never touches the
	// GitHub-concrete remediationProvider helpers. Every GitHub path below stays
	// byte-identical — the ADO behavior is a wholly separate function reached only
	// on this switch, mirroring runPRSelectADO / runGatherSiblingContextADO.
	if repo.Provider == providers.ProviderADO {
		return runGatherPRContextADO(root, repo, stdout, stderr)
	}
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider, err := remediationStageProvider(root, repo, prToken, true)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	base := providerInput("base", providerBaseBranch())
	headPrefix := providerInput("headPrefix", providerBranchNamespace())

	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix, SkipCheckState: true,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, remediationBriefResultFile)
	}
	handoffNumber := providerInput("selectedNumber", "")
	claimedNumber, hasExistingClaim, err := claimedPullRequestNumber(root)
	if err != nil {
		pf(stderr, "error: resolve this run's existing PR claim: %v\n", err)
		return 1
	}
	hasPinnedCandidate := hasExistingClaim
	if handoffNumber != "" {
		selectedNumber, parseErr := strconv.Atoi(handoffNumber)
		if parseErr != nil || selectedNumber <= 0 {
			pf(stderr, "error: selectedNumber input %q must be a positive integer\n", handoffNumber)
			return 1
		}
		if hasExistingClaim && claimedNumber != selectedNumber {
			pf(stderr, "error: selectedNumber input PR #%d does not match this run's claimed PR #%d\n", selectedNumber, claimedNumber)
			return 1
		}
		claimedNumber = selectedNumber
		hasPinnedCandidate = true
	}
	if hasPinnedCandidate {
		var claimed []providers.PullRequestSummary
		for _, pr := range prs {
			if pr.Number == claimedNumber {
				claimed = append(claimed, pr)
				break
			}
		}
		if len(claimed) == 0 {
			return writeNoWorkResult(stdout, stderr, fmt.Sprintf("this run's claimed PR #%d is no longer open", claimedNumber))
		}
		prs = claimed
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
	// #872/#1007: a PR whose head branch is still checked out by another live
	// worktree (its originating implementation run's ci-poll stage, holding the
	// branch while it polls CI on the PR it just opened) cannot be checked out
	// here — git forbids the same branch in two worktrees of the shared managed
	// mirror. De-select such a PR this tick so gather-pr-context skips it
	// cleanly (no claim, no failed run) rather than claiming it and colliding on
	// the checkout every ~60s until the owning run releases the branch; normal
	// selection resumes automatically once it does. Enumerated once here (a
	// local git query, no provider call) and reused across the candidate loop.
	heldBranches := worktreeHeldBranches(".")

	if err := resolveRemediationCheckStates(ctx, provider, repo, prs); err != nil {
		return failProviderStage(stderr, "resolve remediation check states", err, remediationBriefResultFile)
	}
	nonBlocked, blockedDependents, err := filterRemediationPullRequests(ctx, provider, repo, prs, heldBranches)
	if err != nil {
		return failProviderStage(stderr, "filter remediation candidates", err, remediationBriefResultFile)
	}

	// update-behind-pr already selected a full-remediation candidate and threads
	// its number through the workflow. The claim ledger remains the durable
	// fallback across retries and resumes.
	candidates := nonBlocked
	var behindBase func(providers.PullRequestSummary) (bool, error)
	if !hasPinnedCandidate {
		nonBlocked, err = stageClaimAvailablePullRequests(
			root, repo, os.Getenv(executor.RunIDEnvVar), nonBlocked, time.Now(),
		)
		if err != nil {
			return failProviderStage(stderr, "filter claimed remediation candidates", err, remediationBriefResultFile)
		}
		fetchedBases := make(map[string]bool)
		behindBase = func(pr providers.PullRequestSummary) (bool, error) {
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
		}
		candidates, _, err = selectRemediationCandidates(nonBlocked, blockedDependents, behindBase)
		if err != nil {
			pf(stderr, "error: determine remediation eligibility: %v\n", err)
			return 1
		}
	}
	if len(candidates) == 0 {
		return writeNoWorkResult(stdout, stderr, "no PR needs remediation this cycle")
	}

	claimed, err := claimEligiblePullRequestInOrder(root, repo, candidates)
	if err != nil {
		pf(stderr, "error: claim eligible PR: %v\n", err)
		return 1
	}
	if claimed == nil {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}
	selected := *claimed
	if err := resolveRemediationCheckState(ctx, provider, repo, &selected); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("check state for PR #%d", selected.Number), err, remediationBriefResultFile)
	}

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
		return failProviderStage(stderr, fmt.Sprintf("list comments on PR #%d", selected.Number), err, remediationBriefResultFile)
	}
	verdictAuthor, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return failProviderStage(stderr, "resolve merge-review verdict author", err, remediationBriefResultFile)
	}
	verdict := gatherPRVerdict(rawComments, verdictAuthor)

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
	if remState, priorCommentID, ok := latestRemediationStateForPR(selected.Body, rawComments); ok && remState.Escalated && remState.LastDiffDigest != "" {
		digest, derr := diffDigest(".", selected.BaseSHA)
		if derr != nil {
			pf(stderr, "error: compute diff digest for PR #%d: %v\n", selected.Number, derr)
			return 1
		}
		if digest == remState.LastDiffDigest {
			l := layoutFor(root)
			signature := remediationNoopSignature{HeadSHA: selected.HeadSHA, DiffDigest: digest}
			record, operatorReset, err := recordGatherPRContextDigestNoop(
				l, selected.Number, signature, os.Getenv(executor.RunIDEnvVar),
				hasAnyLabel(selected.Labels, []string{remediationEscalatedLabel}),
			)
			if err != nil {
				pf(stderr, "error: record unchanged remediation digest for PR #%d: %v\n", selected.Number, err)
				return 1
			}
			if operatorReset {
				pf(stdout, "PR #%d: escalation cleared by an operator — bypassing the unchanged-digest guard for a fresh remediation attempt\n", selected.Number)
			} else if record.Attempts >= remediationNoopLimit {
				liveBaseTip, err := provider.BranchTipSHA(ctx, repo, selected.Base)
				if err != nil {
					return failProviderStage(stderr, fmt.Sprintf("resolve base branch %q tip for PR #%d", selected.Base, selected.Number), err, remediationBriefResultFile)
				}
				reason := fmt.Sprintf(
					"gather-pr-context observed the unchanged diff digest %s in %d consecutive runs, so remediation cannot make progress",
					digest, record.Attempts,
				)
				generation := nextEscalationGeneration(remState, selected.HeadSHA)
				remState.Cycles++
				remState.LastDiffDigest = digest
				remState.HeadSHA = selected.HeadSHA
				remState.BaseSHA = selected.BaseSHA
				remState.Escalated = true
				remState.EscalatedReason = reason
				remState.EscalationOutcome = remediationOutcomeDidNotConverge
				remState.RemediationAttempted = false
				remState.AttemptedCauses = nil
				remState.EscalatedHeadSHA = selected.HeadSHA
				remState.EscalatedBaseSHA = liveBaseTip
				remState.EscalationCauses = nil
				remState.EscalationGeneration = generation
				if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
					Repository:   repo,
					ID:           strconv.Itoa(selected.Number),
					AddLabels:    []string{remediationEscalatedLabel},
					RemoveLabels: []string{needsRemediationLabel},
				}); err != nil {
					return failProviderStage(stderr, fmt.Sprintf("park unchanged-digest PR #%d", selected.Number), err, remediationBriefResultFile)
				}
				if err := postOrRecreateRemediationComment(ctx, provider, repo, selected.Number, priorCommentID, renderRemediationComment(remState)); err != nil {
					return failProviderStage(stderr, fmt.Sprintf("record unchanged-digest escalation on PR #%d", selected.Number), err, remediationBriefResultFile)
				}
				gaggle := l.Gaggle()
				if gaggle == "" {
					gaggle = providerGaggle()
				}
				if err := markRemediationNoopParked(l, remediationNoopKey(gaggle, selected.Number)); err != nil {
					pf(stderr, "error: mark unchanged-digest PR #%d parked: %v\n", selected.Number, err)
					return 1
				}
				return writeNoWorkResult(stdout, stderr, fmt.Sprintf("PR #%d was visibly parked after %d unchanged-digest runs", selected.Number, record.Attempts))
			} else {
				return writeNoWorkResult(stdout, stderr, fmt.Sprintf(
					"PR #%d's diff (digest %s) is unchanged since its last recorded escalation — no progress possible this cycle",
					selected.Number, digest,
				))
			}
		}
	}

	comments := make([]apiv1.RemediationThreadComment, 0, len(rawComments))
	integrities := []apiv1.Integrity{selected.Integrity}
	for _, c := range rawComments {
		createdAt := ""
		if c.CreatedAt != nil {
			createdAt = c.CreatedAt.Format(time.RFC3339)
		}
		comments = append(comments, apiv1.RemediationThreadComment{
			Author: c.Author, Body: c.Body, CreatedAt: createdAt, URL: c.URL, Integrity: c.Integrity,
		})
		integrities = append(integrities, c.Integrity)
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
	if verdictHasSubstantiveFindingForPR(verdict, selected.Number, resolveMinSeverity(stderr)) {
		hasSubstantiveFindings = "true"
	}
	hasFailingCI := strconv.FormatBool(selected.CheckState == providers.CheckStateFailing)

	resultFile := providerInput("resultFile", remediationBriefResultFile)
	data, err := json.MarshalIndent(apiv1.RemediationBrief{
		Schema:         apiv1.RemediationBriefVersion,
		Integrity:      apiv1.WeakestIntegrity(integrities...),
		SelectedNumber: strconv.Itoa(selected.Number),
		Head:           selected.Head,
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
		WorkspaceBranch:        selected.Head,
		Base:                   selected.Base,
		IsBehindBase:           behind,
		HasSubstantiveFindings: hasSubstantiveFindings,
		HasFailingCI:           hasFailingCI,
		GatherPRContext: apiv1.RemediationPRContext{
			HeadSHA:  selected.HeadSHA,
			BaseSHA:  selected.BaseSHA,
			Verdict:  verdict,
			Comments: comments,
		},
	}, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal remediation brief: %v\n", err)
		return 1
	}
	if err := validateRemediationBriefJSON(data); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "gathered context for PR #%d (%s): behind=%v, %d comment(s)\n", selected.Number, selected.Head, behind, len(comments))
	return 0
}

// runGatherPRContextADO is gather-pr-context's Azure DevOps branch
// (remediation-wiring-plan §3.1). It mirrors runGatherPRContext's shape, but
// every GitHub-concrete surface the lane leans on is either provider-neutral
// (the claim ledger and the git branch rebind, unchanged here) or rerouted to
// an ADO-specific primitive:
//
//   - Selection stays the existing label tier: ADO's ListPullRequests already
//     surfaces PR labels (§0.4), so remediationPriorityFor reads
//     goobers:needs-remediation off pr.Labels unmodified. The failing-CI tier
//     never fires — ADO's ListPullRequests pins CheckState pending and this
//     branch resolves no per-PR check state (§2.2, the label tier never consults
//     it), so a needs-remediation label is the only signal that admits a PR here.
//   - filterRemediationPullRequests and its escalationStillBlocks /
//     liveBlockedOnSiblingBlockers helpers take the GitHub-concrete
//     remediationProvider *ADOProvider cannot satisfy (§0.1), and read PR
//     comments that are work-item comments on ADO; they are gated OFF. The
//     eligible set is produced directly from the label tier (skip a branch a
//     sibling worktree already holds, and a needs-human PR) with no escalation or
//     blocked-on-sibling filtering — there is no sibling election on ADO.
//   - resolveRemediationCheckState(s) (RefCheckState/RefCheckStates, absent on
//     ADO) is skipped for the same reason.
//   - The merge-review verdict rides on a PR THREAD, not a PR comment (§0.3): it
//     is recovered from ListPullRequestThreadComments (the ADO analog of
//     ListComments) and trusted against AuthenticatedLogin (connectionData).
//
// It never resolves a github:* capability token — the ADO provider draws its own
// org-scoped auth from the shared stage provider factory. repo:push still
// carries the git checkout credential (the #392 branch rebind), provider-neutral
// on every backend.
func runGatherPRContextADO(root string, repo providers.RepositoryRef, stdout, stderr io.Writer) int {
	provider, err := newProviderForStageAs[*providers.ADOProvider](root, repo, false)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Sibling-overlap serialization is a GitHub-only refinement; on Azure DevOps
	// the remediation loop is pure FIFO. State it plainly so an operator is not
	// surprised that two overlapping ADO PRs are remediated independently rather
	// than serialized behind one another.
	pf(stderr, "note: Azure DevOps supports only the %q remediation algorithm; sibling-overlap serialization is unavailable, so pull requests are remediated in strict oldest-first order\n", remediationAlgorithmFIFO)

	base := providerInput("base", providerBaseBranch())
	headPrefix := providerInput("headPrefix", providerBranchNamespace())

	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix, SkipCheckState: true,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, remediationBriefResultFile)
	}
	handoffNumber := providerInput("selectedNumber", "")
	claimedNumber, hasExistingClaim, err := claimedPullRequestNumber(root)
	if err != nil {
		pf(stderr, "error: resolve this run's existing PR claim: %v\n", err)
		return 1
	}
	hasPinnedCandidate := hasExistingClaim
	if handoffNumber != "" {
		selectedNumber, parseErr := strconv.Atoi(handoffNumber)
		if parseErr != nil || selectedNumber <= 0 {
			pf(stderr, "error: selectedNumber input %q must be a positive integer\n", handoffNumber)
			return 1
		}
		if hasExistingClaim && claimedNumber != selectedNumber {
			pf(stderr, "error: selectedNumber input PR #%d does not match this run's claimed PR #%d\n", selectedNumber, claimedNumber)
			return 1
		}
		claimedNumber = selectedNumber
		hasPinnedCandidate = true
	}
	if hasPinnedCandidate {
		var claimed []providers.PullRequestSummary
		for _, pr := range prs {
			if pr.Number == claimedNumber {
				claimed = append(claimed, pr)
				break
			}
		}
		if len(claimed) == 0 {
			return writeNoWorkResult(stdout, stderr, fmt.Sprintf("this run's claimed PR #%d is no longer open", claimedNumber))
		}
		prs = claimed
	}

	heldBranches := worktreeHeldBranches(".")

	// The label-tier eligible set, produced directly (filterRemediationPullRequests
	// is gated OFF, §3.1/§0.1): skip a branch another live worktree already holds
	// (#872/#1007) and a needs-human PR. No escalation / blocked-on-sibling
	// filtering on ADO, so no dependent is crowned.
	nonBlocked := make([]providers.PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		if heldBranches[pr.Head] {
			continue
		}
		if hasAnyLabel(pr.Labels, []string{providers.LabelNeedsHuman}) {
			continue
		}
		nonBlocked = append(nonBlocked, pr)
	}
	blockedDependents := map[int]int{}

	candidates := nonBlocked
	if !hasPinnedCandidate {
		nonBlocked, err = stageClaimAvailablePullRequests(
			root, repo, os.Getenv(executor.RunIDEnvVar), nonBlocked, time.Now(),
		)
		if err != nil {
			return failProviderStage(stderr, "filter claimed remediation candidates", err, remediationBriefResultFile)
		}
		// The behind-base fallback tier crowns only a lander with a live parked
		// dependent, which never materializes on ADO (no sibling election), so
		// selectRemediationCandidates never invokes this probe — it exists solely
		// to satisfy the signature and fails loudly if the invariant is ever
		// broken. resolveRemediationCheckStates (RefCheckStates, absent on ADO) is
		// deliberately not run: the label tier never consults CheckState (§2.2).
		behindBase := func(pr providers.PullRequestSummary) (bool, error) {
			return false, fmt.Errorf("behind-base probe is unreachable on ADO (no sibling election) for PR #%d", pr.Number)
		}
		candidates, _, err = selectRemediationCandidates(nonBlocked, blockedDependents, behindBase)
		if err != nil {
			pf(stderr, "error: determine remediation eligibility: %v\n", err)
			return 1
		}
	}
	if len(candidates) == 0 {
		return writeNoWorkResult(stdout, stderr, "no PR needs remediation this cycle")
	}

	claimed, err := claimEligiblePullRequestInOrder(root, repo, candidates)
	if err != nil {
		pf(stderr, "error: claim eligible PR: %v\n", err)
		return 1
	}
	if claimed == nil {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}
	selected := *claimed
	// resolveRemediationCheckState (RefCheckState, absent on ADO) is skipped:
	// selected.CheckState stays pending from ListPullRequests, the label tier does
	// not consult it, and hasFailingCI below reports false accordingly (§2.2).

	if _, err := checkoutExistingBranch(".", selected.Head, pushToken); err != nil {
		pf(stderr, "error: checkout PR #%d's branch %q: %v\n", selected.Number, selected.Head, err)
		return 1
	}

	behind, err := isBehindBase(".", selected.BaseSHA)
	if err != nil {
		pf(stderr, "error: check base ancestry for PR #%d: %v\n", selected.Number, err)
		return 1
	}

	// The verdict apply-verdict posted rides on a PR THREAD, not a PR comment
	// (§0.3): recover it from ListPullRequestThreadComments (the ADO analog of
	// ListComments, which addresses work-item comments on ADO) and trust it
	// against AuthenticatedLogin — connectionData returns the same displayName the
	// thread author is mapped to, so a thread we posted is recognized.
	rawComments, err := provider.ListPullRequestThreadComments(ctx, repo, strconv.Itoa(selected.Number))
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("list thread comments on PR #%d", selected.Number), err, remediationBriefResultFile)
	}
	verdictAuthor, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return failProviderStage(stderr, "resolve merge-review verdict author", err, remediationBriefResultFile)
	}
	verdict := gatherPRVerdict(rawComments, verdictAuthor)

	// Digest short-circuit (#716), identical to the GitHub path but with the
	// sticky remediation-state carried on the PR thread. selected.Body is empty on
	// ADO (ListPullRequests does not populate it), so the state is recovered from
	// the thread comments alone.
	if remState, priorCommentID, ok := latestRemediationStateForPR(selected.Body, rawComments); ok && remState.Escalated && remState.LastDiffDigest != "" {
		digest, derr := diffDigest(".", selected.BaseSHA)
		if derr != nil {
			pf(stderr, "error: compute diff digest for PR #%d: %v\n", selected.Number, derr)
			return 1
		}
		if digest == remState.LastDiffDigest {
			l := layoutFor(root)
			signature := remediationNoopSignature{HeadSHA: selected.HeadSHA, DiffDigest: digest}
			record, operatorReset, err := recordGatherPRContextDigestNoop(
				l, selected.Number, signature, os.Getenv(executor.RunIDEnvVar),
				hasAnyLabel(selected.Labels, []string{remediationEscalatedLabel}),
			)
			if err != nil {
				pf(stderr, "error: record unchanged remediation digest for PR #%d: %v\n", selected.Number, err)
				return 1
			}
			if operatorReset {
				pf(stdout, "PR #%d: escalation cleared by an operator — bypassing the unchanged-digest guard for a fresh remediation attempt\n", selected.Number)
			} else if record.Attempts >= remediationNoopLimit {
				reason := fmt.Sprintf(
					"gather-pr-context observed the unchanged diff digest %s in %d consecutive runs, so remediation cannot make progress",
					digest, record.Attempts,
				)
				generation := nextEscalationGeneration(remState, selected.HeadSHA)
				remState.Cycles++
				remState.LastDiffDigest = digest
				remState.HeadSHA = selected.HeadSHA
				remState.BaseSHA = selected.BaseSHA
				remState.Escalated = true
				remState.EscalatedReason = reason
				remState.EscalationOutcome = remediationOutcomeDidNotConverge
				remState.RemediationAttempted = false
				remState.AttemptedCauses = nil
				remState.EscalatedHeadSHA = selected.HeadSHA
				remState.EscalatedBaseSHA = selected.BaseSHA
				remState.EscalationCauses = nil
				remState.EscalationGeneration = generation
				pullID := strconv.Itoa(selected.Number)
				if err := provider.AddPullRequestLabels(ctx, repo, pullID, []string{remediationEscalatedLabel}); err != nil {
					return failProviderStage(stderr, fmt.Sprintf("park unchanged-digest PR #%d", selected.Number), err, remediationBriefResultFile)
				}
				if err := provider.RemovePullRequestLabel(ctx, repo, pullID, needsRemediationLabel); err != nil {
					return failProviderStage(stderr, fmt.Sprintf("clear needs-remediation label from PR #%d", selected.Number), err, remediationBriefResultFile)
				}
				if err := postOrRecreateRemediationThreadComment(ctx, provider, repo, pullID, priorCommentID, renderRemediationComment(remState)); err != nil {
					return failProviderStage(stderr, fmt.Sprintf("record unchanged-digest escalation on PR #%d", selected.Number), err, remediationBriefResultFile)
				}
				gaggle := l.Gaggle()
				if gaggle == "" {
					gaggle = providerGaggle()
				}
				if err := markRemediationNoopParked(l, remediationNoopKey(gaggle, selected.Number)); err != nil {
					pf(stderr, "error: mark unchanged-digest PR #%d parked: %v\n", selected.Number, err)
					return 1
				}
				return writeNoWorkResult(stdout, stderr, fmt.Sprintf("PR #%d was visibly parked after %d unchanged-digest runs", selected.Number, record.Attempts))
			} else {
				return writeNoWorkResult(stdout, stderr, fmt.Sprintf(
					"PR #%d's diff (digest %s) is unchanged since its last recorded escalation — no progress possible this cycle",
					selected.Number, digest,
				))
			}
		}
	}

	comments := make([]apiv1.RemediationThreadComment, 0, len(rawComments))
	integrities := []apiv1.Integrity{selected.Integrity}
	for _, c := range rawComments {
		createdAt := ""
		if c.CreatedAt != nil {
			createdAt = c.CreatedAt.Format(time.RFC3339)
		}
		comments = append(comments, apiv1.RemediationThreadComment{
			Author: c.Author, Body: c.Body, CreatedAt: createdAt, URL: c.URL, Integrity: c.Integrity,
		})
		integrities = append(integrities, c.Integrity)
	}

	hasSubstantiveFindings := "false"
	if verdictHasSubstantiveFindingForPR(verdict, selected.Number, resolveMinSeverity(stderr)) {
		hasSubstantiveFindings = "true"
	}
	hasFailingCI := strconv.FormatBool(selected.CheckState == providers.CheckStateFailing)

	resultFile := providerInput("resultFile", remediationBriefResultFile)
	data, err := json.MarshalIndent(apiv1.RemediationBrief{
		Schema:                 apiv1.RemediationBriefVersion,
		Integrity:              apiv1.WeakestIntegrity(integrities...),
		SelectedNumber:         strconv.Itoa(selected.Number),
		Head:                   selected.Head,
		WorkspaceBranch:        selected.Head,
		Base:                   selected.Base,
		IsBehindBase:           behind,
		HasSubstantiveFindings: hasSubstantiveFindings,
		HasFailingCI:           hasFailingCI,
		GatherPRContext: apiv1.RemediationPRContext{
			HeadSHA:  selected.HeadSHA,
			BaseSHA:  selected.BaseSHA,
			Verdict:  verdict,
			Comments: comments,
		},
	}, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal remediation brief: %v\n", err)
		return 1
	}
	if err := validateRemediationBriefJSON(data); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "gathered context for PR #%d (%s): behind=%v, %d comment(s)\n", selected.Number, selected.Head, behind, len(comments))
	return 0
}

// gatherPRVerdict prefers the oldest trusted marked comment because
// reconciliation keeps that comment as the canonical status. Before marked
// comments existed, each review appended a new comment, so the migration
// fallback uses the newest parseable verdict from the trusted author.
func gatherPRVerdict(comments []providers.Comment, author string) *apiv1.Verdict {
	var legacy *apiv1.Verdict
	for _, comment := range comments {
		if !isTrustedMergeReviewAuthor(comment.Author, author) {
			continue
		}
		candidate, ok := parseVerdictComment(comment.Body)
		if isMergeReviewStatusComment(comment.Body) {
			if !ok {
				return nil
			}
			return &candidate
		}
		if ok {
			legacy = &candidate
		}
	}
	return legacy
}

// filterRemediationPullRequests applies the shared exclusion rules before
// either the API fast lane or the full worktree-backed remediation path selects
// a candidate. heldBranches is nil for the API-only lane, which can safely
// update a branch even while another local worktree has it checked out. The
// returned counts identify eligible crowned landers from their live parked
// dependents without a second provider scan.
func filterRemediationPullRequests(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prs []providers.PullRequestSummary, heldBranches map[string]bool) ([]providers.PullRequestSummary, map[int]int, error) {
	var eligible []providers.PullRequestSummary
	blockedDependents := make(map[int]int)
	for _, pr := range prs {
		if heldBranches[pr.Head] {
			continue
		}
		if hasAnyLabel(pr.Labels, []string{providers.LabelNeedsHuman}) {
			continue
		}
		blocked, err := escalationStillBlocks(ctx, provider, repo, pr)
		if err != nil {
			return nil, nil, fmt.Errorf("check escalation state for PR #%d: %w", pr.Number, err)
		}
		if blocked {
			continue
		}
		blockedState, err := liveBlockedOnSiblingState(ctx, provider, repo, pr)
		if err != nil {
			return nil, nil, fmt.Errorf("check blocked-on-sibling state for PR #%d: %w", pr.Number, err)
		}
		liveBlockers := blockedState.Blockers
		for _, blocker := range liveBlockers {
			blockedDependents[blocker]++
		}
		if shouldParkRemediation(pr, blockedState) {
			continue
		}
		eligible = append(eligible, pr)
	}
	return eligible, blockedDependents, nil
}

func shouldParkRemediation(pr providers.PullRequestSummary, blockedState blockedOnSiblingState) bool {
	if len(blockedState.Blockers) == 0 {
		return false
	}
	if pr.CheckState == providers.CheckStateFailing {
		return false
	}
	if strings.HasPrefix(blockedState.Reason, "foundation-coupled to PR #") {
		return true
	}
	return !hasAnyLabel(pr.Labels, []string{needsRemediationLabel})
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

func resolveRemediationCheckState(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, pr *providers.PullRequestSummary) error {
	if pr.CheckState != "" {
		return nil
	}
	checkState, err := provider.RefCheckState(ctx, repo, pr.HeadSHA)
	if err != nil {
		return err
	}
	pr.CheckState = checkState
	return nil
}

func resolveRemediationCheckStates(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prs []providers.PullRequestSummary) error {
	refs := make([]string, 0, len(prs))
	seen := make(map[string]bool, len(prs))
	for _, pr := range prs {
		if pr.CheckState == "" && !seen[pr.HeadSHA] {
			refs = append(refs, pr.HeadSHA)
			seen[pr.HeadSHA] = true
		}
	}
	states, err := provider.RefCheckStates(ctx, repo, refs)
	if err != nil {
		return err
	}
	for i := range prs {
		if prs[i].CheckState != "" {
			continue
		}
		state, ok := states[prs[i].HeadSHA]
		if !ok {
			return fmt.Errorf("check state for ref %q was not returned", prs[i].HeadSHA)
		}
		prs[i].CheckState = state
	}
	return nil
}

// selectRemediationCandidates returns every open PR carrying a strong
// remediation signal, ordered by tier (needs-remediation, then failing CI) and
// PR number. Offering all tiers to the claim ledger lets concurrent runs fall
// through when stronger candidates are already claimed instead of leaving
// lower-tier eligible work idle.
//
// Only when no PR clears either strong tier does a crowned lander merely behind its
// base become eligible. A crown is materialized by at least one live parked
// dependent naming the PR as a blocker. This keeps the rest of an overlapping
// wave parked until its predecessor lands instead of eagerly rebasing every
// behind-base sibling after each merge. Checking "behind base" requires
// fetching candidate branches, so behindBase is invoked only for crowns when
// nothing stronger exists.
func selectRemediationCandidates(prs []providers.PullRequestSummary, blockedDependents map[int]int, behindBase func(providers.PullRequestSummary) (bool, error)) ([]providers.PullRequestSummary, remediationPriority, error) {
	candidates, best := strongRemediationCandidates(prs)
	if len(candidates) > 0 {
		return candidates, best, nil
	}

	for _, pr := range prs {
		// Same rationale as remediationPriorityFor: escalation exclusion
		// already happened upstream (self-heal-aware), so no re-check here.
		if blockedDependents[pr.Number] == 0 {
			continue
		}
		behind, err := behindBase(pr)
		if err != nil {
			return nil, remediationPriorityNone, err
		}
		if behind {
			candidates = append(candidates, pr)
		}
	}
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Number < candidates[j].Number
		})
		best = remediationPriorityBehindBase
	}
	return candidates, best, nil
}

func strongRemediationCandidates(prs []providers.PullRequestSummary) ([]providers.PullRequestSummary, remediationPriority) {
	byPriority := map[remediationPriority][]providers.PullRequestSummary{}
	best := remediationPriorityNone
	for _, pr := range prs {
		priority := remediationPriorityFor(pr)
		if priority == remediationPriorityNone {
			continue
		}
		byPriority[priority] = append(byPriority[priority], pr)
		if priority > best {
			best = priority
		}
	}
	for _, priority := range []remediationPriority{
		remediationPriorityNeedsRemediation,
		remediationPriorityFailingCI,
	} {
		tier := byPriority[priority]
		sort.Slice(tier, func(i, j int) bool {
			return tier[i].Number < tier[j].Number
		})
		byPriority[priority] = tier
	}
	candidates := append([]providers.PullRequestSummary(nil), byPriority[remediationPriorityNeedsRemediation]...)
	candidates = append(candidates, byPriority[remediationPriorityFailingCI]...)
	return candidates, best
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
//
// minSeverity applies the declared remediation policy's severity floor
// (issue #941/PRR-6, gate-time filtering): a finding below the floor never
// makes this report true, but is NOT dropped from the brief — the full
// verdict (every finding, any severity) still reaches the agentic context
// via GatherPRContext.Verdict, so a sub-threshold finding remains visible
// evidence, it just cannot by itself burn a remediation cycle.
// resolveMinSeverity reads the declared minSeverity policy input (#941/
// PRR-6), defaulting to apiv1.SeverityInfo — the liberal floor that counts
// every substantive finding, reproducing today's behavior when the input is
// unset. An unrecognized value falls back to the same liberal default rather
// than silently ranking as "meets nothing" (Severity.Rank()'s own default),
// since a typo'd policy value should fail open to today's behavior, not
// closed to never-remediate.
func resolveMinSeverity(stderr io.Writer) apiv1.Severity {
	raw := providerInput("minSeverity", string(apiv1.SeverityInfo))
	switch apiv1.Severity(raw) {
	case apiv1.SeverityInfo, apiv1.SeverityWarning, apiv1.SeverityError, apiv1.SeverityCritical:
		return apiv1.Severity(raw)
	default:
		pf(stderr, "warning: minSeverity %q is not one of info/warning/error/critical; using %q\n", raw, apiv1.SeverityInfo)
		return apiv1.SeverityInfo
	}
}

// remediationAlgorithmFIFO selects the oldest eligible PR (lowest number) first.
// It is the only supported remediation algorithm today and the default, mirroring
// the electionPolicy "fifo" default: the safe, boring, fully-reproducible order.
const remediationAlgorithmFIFO = "fifo"

// validateRemediationAlgorithm checks the configured PR-selection algorithm for
// the remediation loop. Exposing it as an explicit, named input ("fifo") makes
// the selection order a declared, provider-portable contract rather than
// incidental behaviour. FIFO is the only value accepted today; an unknown value
// warns and is treated as fifo (never a hard failure — mirrors resolveMinSeverity
// and resolveElectionPolicy). Honouring it is a no-op on the selection path,
// which already orders same-tier candidates by ascending PR number.
func validateRemediationAlgorithm(stderr io.Writer) {
	if raw := providerInput("remediationAlgorithm", remediationAlgorithmFIFO); raw != remediationAlgorithmFIFO {
		pf(stderr, "warning: remediationAlgorithm %q is not supported (only %q); using %q\n",
			raw, remediationAlgorithmFIFO, remediationAlgorithmFIFO)
	}
}

func verdictHasSubstantiveFindingForPR(verdict *apiv1.Verdict, prNumber int, minSeverity apiv1.Severity) bool {
	if verdict == nil {
		return false
	}
	target := strconv.Itoa(prNumber)
	for _, finding := range verdict.Findings {
		if substantiveFindingAppliesToPR(finding, target, minSeverity) {
			return true
		}
	}
	return false
}

func substantiveFindingAppliesToPR(finding apiv1.Finding, target string, minSeverity apiv1.Severity) bool {
	// A finding's class routes it to the right remediation action, so an
	// explicitly non-code-change class (cross-pr-blocked, rebase-needed) is not
	// a substantive-rework signal. But an UNSET class is treated liberally — the
	// same liberal default the unset-Severity branch below uses — because a
	// reviewer that omits the tag must not silently drop a real defect from
	// remediation. (A live ADO command-injection finding carried no class and
	// stalled the whole loop until this backstop existed; the severity floor
	// still filters low-value classless noise.)
	if finding.Class != "" && !finding.Class.RequiresCodeChange() {
		return false
	}
	// An unset Severity (verdicts recorded before this field existed, or
	// any evaluator that never populates it) always counts — the liberal
	// default must reproduce today's behavior exactly.
	if finding.Severity != "" && finding.Severity.Rank() < minSeverity.Rank() {
		return false
	}
	locationRefs := prReferencePattern.FindAllStringSubmatch(finding.Location, -1)
	if len(locationRefs) == 0 || referencesTarget(locationRefs, target) {
		return true
	}
	return referencesTarget(hashReferencePattern.FindAllStringSubmatch(finding.Message, -1), target)
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

// worktreeHeldBranches returns the set of branch short-names currently checked
// out by a git worktree at dir's repository OTHER than the worktree at dir
// itself. pr-remediation's stages run in a worktree that shares one managed
// mirror clone (internal/worktree.Manager) with every other in-flight run, and
// git refuses to check out a branch a different live worktree already holds.
//
// A freshly-opened implementation PR's head branch
// (goobers/implementation/<runId>) is still held by its OWN originating run's
// ci-poll worktree while that run polls GitHub CI on the PR it just opened, so
// checking it out here collides ("fatal: '<branch>' is already used by
// worktree at ...") and fails on every ~60s retry until the owning run
// releases it — #872 / #1007. Enumerating the held branches up front lets
// runGatherPRContext de-select such a PR for this tick (a clean no-work skip
// that resumes normal selection once the owning run finishes) instead of
// claiming it and failing the checkout every cycle.
//
// Best-effort by design: it must only ever PREVENT a checkout already
// guaranteed to collide, never block an otherwise-fine one. If the worktree
// list cannot be enumerated (e.g. dir is not inside a git repo — as in
// selection-only unit paths), it returns an empty set and no error, leaving
// behavior exactly as it was before this guard existed. The worktree at dir is
// excluded from the result: `git checkout -B` on our own current branch never
// collides.
func worktreeHeldBranches(dir string) map[string]bool {
	held := make(map[string]bool)

	self := exec.Command("git", "rev-parse", "--show-toplevel")
	self.Dir = dir
	selfOut, err := self.Output()
	if err != nil {
		return held // not a git worktree (or unreadable): nothing to guard against
	}
	selfPath := canonicalPath(strings.TrimSpace(string(selfOut)))

	list := exec.Command("git", "worktree", "list", "--porcelain")
	list.Dir = dir
	out, err := list.Output()
	if err != nil {
		return held
	}
	// Porcelain output is newline-separated records: a "worktree <path>" line
	// begins each record, an optional "branch refs/heads/<name>" line names the
	// branch it holds (absent for a bare mirror or a detached checkout).
	var isSelf bool
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			wtPath := canonicalPath(strings.TrimSpace(strings.TrimPrefix(line, "worktree ")))
			isSelf = wtPath == selfPath
		case strings.HasPrefix(line, "branch "):
			if isSelf {
				continue // our own worktree — checking out our own branch never collides
			}
			branch := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "branch ")), "refs/heads/")
			if branch != "" {
				held[branch] = true
			}
		}
	}
	return held
}

// canonicalPath resolves symlinks in p so two spellings of the same directory
// compare equal — on macOS a worktree recorded under /var/folders and the same
// path reported by `git rev-parse --show-toplevel` as /private/var/folders must
// not be mistaken for two different worktrees. Falls back to the input on any
// resolution error (e.g. the path no longer exists), which is safe: a failed
// self-match at worst leaves our own already-checked-out branch in the held
// set, and that branch is never a remediation candidate's head.
func canonicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
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
