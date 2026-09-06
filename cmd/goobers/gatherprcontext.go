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
	"neither stronger signal is present. A run dispatched for one pull\n" +
	"request (goobers run --pr, or a pull_request webhook delivery) selects\n" +
	"that PR and no other, and reports no-work naming the reason when it is\n" +
	"not selectable. Check out its branch into this stage's worktree and\n" +
	"load the latest merge-review verdict + PR-thread\n" +
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
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	validateRemediationAlgorithm(stderr)
	adapter, err := newGatherPRContextAdapter(root, repo)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return runGatherPRContextCore(root, repo, adapter, stdout, stderr)
}

// gatherPRContextFeatures explicitly records evidence the decision core may
// consume. ADO gaps remain absent rather than being emulated by stub calls.
type gatherPRContextFeatures struct{ checkState, siblingBlocking, liveBaseTip bool }

type gatherPRContextAdapter struct {
	features         gatherPRContextFeatures
	pushToken, note  string
	list             func(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
	prepare          func(context.Context, []providers.PullRequestSummary, map[string]bool) ([]providers.PullRequestSummary, map[int]int, error)
	resolveCheck     func(context.Context, *providers.PullRequestSummary) error
	behindBase       func(providers.PullRequestSummary) (bool, error)
	comments         func(context.Context, string) ([]providers.Comment, error)
	commentOperation func(int) string
	login            func(context.Context) (string, error)
	baseTip          func(context.Context, string) (string, error)
	park             func(context.Context, providers.PullRequestSummary, string, string) error
}
type gatherPRContextAdapterError struct {
	operation string
	err       error
}

func (e *gatherPRContextAdapterError) Error() string { return e.operation + ": " + e.err.Error() }
func (e *gatherPRContextAdapterError) Unwrap() error { return e.err }
func gatherPRAdapterError(operation string, err error) error {
	return &gatherPRContextAdapterError{operation, err}
}
func failGatherPRAdapter(stderr io.Writer, err error) int {
	var e *gatherPRContextAdapterError
	if errors.As(err, &e) {
		return failProviderStage(stderr, e.operation, e.err, remediationBriefResultFile)
	}
	return failProviderStage(stderr, "prepare remediation candidates", err, remediationBriefResultFile)
}
func newGatherPRContextAdapter(root string, repo providers.RepositoryRef) (gatherPRContextAdapter, error) {
	if repo.Provider == providers.ProviderADO {
		return newADOGatherPRContextAdapter(root, repo)
	}
	return newGitHubGiteaGatherPRContextAdapter(root, repo)
}
func newGitHubGiteaGatherPRContextAdapter(root string, repo providers.RepositoryRef) (gatherPRContextAdapter, error) {
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return gatherPRContextAdapter{}, err
	}
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		return gatherPRContextAdapter{}, err
	}
	provider, err := remediationStageProvider(root, repo, prToken, true)
	if err != nil {
		return gatherPRContextAdapter{}, err
	}
	bases := map[string]bool{}
	return gatherPRContextAdapter{
		features: gatherPRContextFeatures{checkState: true, siblingBlocking: true, liveBaseTip: true}, pushToken: pushToken, list: provider.ListPullRequests,
		prepare: func(ctx context.Context, prs []providers.PullRequestSummary, held map[string]bool) ([]providers.PullRequestSummary, map[int]int, error) {
			if err := resolveRemediationCheckStates(ctx, provider, repo, prs); err != nil {
				return nil, nil, gatherPRAdapterError("resolve remediation check states", err)
			}
			eligible, blocked, err := filterRemediationPullRequests(ctx, provider, repo, prs, held)
			if err != nil {
				return nil, nil, gatherPRAdapterError("filter remediation candidates", err)
			}
			return eligible, blocked, nil
		},
		resolveCheck: func(ctx context.Context, pr *providers.PullRequestSummary) error {
			return resolveRemediationCheckState(ctx, provider, repo, pr)
		},
		behindBase: func(pr providers.PullRequestSummary) (bool, error) {
			if !bases[pr.Base] {
				if _, err := fetchExistingBranch(".", pr.Base, pushToken); err != nil {
					return false, fmt.Errorf("fetch base branch %q: %w", pr.Base, err)
				}
				bases[pr.Base] = true
			}
			head, err := fetchExistingBranch(".", pr.Head, pushToken)
			if err != nil {
				return false, fmt.Errorf("fetch PR #%d branch %q: %w", pr.Number, pr.Head, err)
			}
			return isCommitBehindBase(".", pr.BaseSHA, head)
		},
		comments: func(ctx context.Context, id string) ([]providers.Comment, error) {
			return provider.ListComments(ctx, repo, id)
		}, commentOperation: func(n int) string { return fmt.Sprintf("list comments on PR #%d", n) }, login: provider.AuthenticatedLogin,
		baseTip: func(ctx context.Context, branch string) (string, error) {
			return provider.BranchTipSHA(ctx, repo, branch)
		},
		park: func(ctx context.Context, pr providers.PullRequestSummary, prior, body string) error {
			if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{Repository: repo, ID: strconv.Itoa(pr.Number), AddLabels: []string{remediationEscalatedLabel}, RemoveLabels: []string{needsRemediationLabel}}); err != nil {
				return gatherPRAdapterError(fmt.Sprintf("park unchanged-digest PR #%d", pr.Number), err)
			}
			if err := postOrRecreateRemediationComment(ctx, provider, repo, pr.Number, prior, body); err != nil {
				return gatherPRAdapterError(fmt.Sprintf("record unchanged-digest escalation on PR #%d", pr.Number), err)
			}
			return nil
		},
	}, nil
}
func newADOGatherPRContextAdapter(root string, repo providers.RepositoryRef) (gatherPRContextAdapter, error) {
	provider, err := newProviderForStageAs[*providers.ADOProvider](root, repo, false)
	if err != nil {
		return gatherPRContextAdapter{}, err
	}
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		return gatherPRContextAdapter{}, err
	}
	return gatherPRContextAdapter{pushToken: pushToken, note: "note: Azure DevOps supports only the \"fifo\" remediation algorithm; sibling-overlap serialization is unavailable, so pull requests are remediated in strict oldest-first order", list: provider.ListPullRequests,
		prepare: func(_ context.Context, prs []providers.PullRequestSummary, held map[string]bool) ([]providers.PullRequestSummary, map[int]int, error) {
			eligible := make([]providers.PullRequestSummary, 0, len(prs))
			for _, pr := range prs {
				if !held[pr.Head] && !hasAnyLabel(pr.Labels, []string{providers.LabelNeedsHuman}) {
					eligible = append(eligible, pr)
				}
			}
			return eligible, map[int]int{}, nil
		},
		comments: func(ctx context.Context, id string) ([]providers.Comment, error) {
			return provider.ListPullRequestThreadComments(ctx, repo, id)
		}, commentOperation: func(n int) string { return fmt.Sprintf("list thread comments on PR #%d", n) }, login: provider.AuthenticatedLogin,
		park: func(ctx context.Context, pr providers.PullRequestSummary, prior, body string) error {
			id := strconv.Itoa(pr.Number)
			if err := provider.AddPullRequestLabels(ctx, repo, id, []string{remediationEscalatedLabel}); err != nil {
				return gatherPRAdapterError(fmt.Sprintf("park unchanged-digest PR #%d", pr.Number), err)
			}
			if err := provider.RemovePullRequestLabel(ctx, repo, id, needsRemediationLabel); err != nil {
				return gatherPRAdapterError(fmt.Sprintf("clear needs-remediation label from PR #%d", pr.Number), err)
			}
			if err := postOrRecreateRemediationThreadComment(ctx, provider, repo, id, prior, body); err != nil {
				return gatherPRAdapterError(fmt.Sprintf("record unchanged-digest escalation on PR #%d", pr.Number), err)
			}
			return nil
		},
	}, nil
}
func runGatherPRContextCore(root string, repo providers.RepositoryRef, a gatherPRContextAdapter, stdout, stderr io.Writer) int {
	if a.note != "" {
		pf(stderr, "%s\n", a.note)
	}
	base := providerInput("base", providerBaseBranch())
	prefix := providerInput("headPrefix", providerBranchNamespace())
	target := remediationTargetFromEnv()
	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := a.list(ctx, providers.ListPullRequestsRequest{Repository: repo, Base: base, HeadPrefix: prefix, SkipCheckState: true})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, remediationBriefResultFile)
	}
	listed := prs
	prs, pinned, done, code := gatherPRContextCandidateScope(root, target, prs, stdout, stderr)
	if done {
		return code
	}
	eligible, blocked, err := a.prepare(ctx, prs, worktreeHeldBranches("."))
	if err != nil {
		return failGatherPRAdapter(stderr, err)
	}
	selected, done, code := gatherPRContextSelectCandidate(gatherPRContextCandidateSelection{root: root, repo: repo, base: base, headPrefix: prefix, target: target, listed: listed, eligible: eligible, blockedDependents: blocked, hasPinnedCandidate: pinned, supportsBehindBaseFallback: a.features.siblingBlocking, behindBase: a.behindBase}, stdout, stderr)
	if done {
		return code
	}
	if a.features.checkState {
		if err := a.resolveCheck(ctx, &selected); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("check state for PR #%d", selected.Number), err, remediationBriefResultFile)
		}
	}
	if _, err := checkoutExistingBranch(".", selected.Head, a.pushToken); err != nil {
		pf(stderr, "error: checkout PR #%d's branch %q: %v\n", selected.Number, selected.Head, err)
		return 1
	}
	behind, err := isBehindBase(".", selected.BaseSHA)
	if err != nil {
		pf(stderr, "error: check base ancestry for PR #%d: %v\n", selected.Number, err)
		return 1
	}
	comments, err := a.comments(ctx, strconv.Itoa(selected.Number))
	if err != nil {
		return failProviderStage(stderr, a.commentOperation(selected.Number), err, remediationBriefResultFile)
	}
	author, err := a.login(ctx)
	if err != nil {
		return failProviderStage(stderr, "resolve merge-review verdict author", err, remediationBriefResultFile)
	}
	if handled, code := handleGatherPRContextUnchangedDigest(root, a, ctx, selected, comments, stdout, stderr); handled {
		return code
	}
	return writeGatherPRContextResult(selected, behind, gatherPRVerdict(comments, author), comments, stdout, stderr)
}
func handleGatherPRContextUnchangedDigest(root string, a gatherPRContextAdapter, ctx context.Context, pr providers.PullRequestSummary, comments []providers.Comment, stdout, stderr io.Writer) (bool, int) {
	state, prior, ok := latestRemediationStateForPR(pr.Body, comments)
	if !ok || !state.Escalated || state.LastDiffDigest == "" {
		return false, 0
	}
	digest, err := diffDigest(".", pr.BaseSHA)
	if err != nil {
		pf(stderr, "error: compute diff digest for PR #%d: %v\n", pr.Number, err)
		return true, 1
	}
	if digest != state.LastDiffDigest {
		return false, 0
	}
	l := layoutFor(root)
	record, reset, err := recordGatherPRContextDigestNoop(l, pr.Number, remediationNoopSignature{HeadSHA: pr.HeadSHA, DiffDigest: digest}, os.Getenv(executor.RunIDEnvVar), hasAnyLabel(pr.Labels, []string{remediationEscalatedLabel}))
	if err != nil {
		pf(stderr, "error: record unchanged remediation digest for PR #%d: %v\n", pr.Number, err)
		return true, 1
	}
	if reset {
		pf(stdout, "PR #%d: escalation cleared by an operator — bypassing the unchanged-digest guard for a fresh remediation attempt\n", pr.Number)
		return false, 0
	}
	if record.Attempts < remediationNoopLimit {
		return true, writeNoWorkResult(stdout, stderr, fmt.Sprintf("PR #%d's diff (digest %s) is unchanged since its last recorded escalation — no progress possible this cycle", pr.Number, digest))
	}
	base := pr.BaseSHA
	if a.features.liveBaseTip {
		base, err = a.baseTip(ctx, pr.Base)
		if err != nil {
			return true, failProviderStage(stderr, fmt.Sprintf("resolve base branch %q tip for PR #%d", pr.Base, pr.Number), err, remediationBriefResultFile)
		}
	}
	state.Cycles++
	state.LastDiffDigest = digest
	state.HeadSHA = pr.HeadSHA
	state.BaseSHA = pr.BaseSHA
	state.Escalated = true
	state.EscalatedReason = fmt.Sprintf("gather-pr-context observed the unchanged diff digest %s in %d consecutive runs, so remediation cannot make progress", digest, record.Attempts)
	state.EscalationOutcome = remediationOutcomeDidNotConverge
	state.RemediationAttempted = false
	state.AttemptedCauses = nil
	state.EscalatedHeadSHA = pr.HeadSHA
	state.EscalatedBaseSHA = base
	state.EscalationCauses = nil
	// Re-confirming an already-parked, still-unchanged head is not a fresh
	// remediation attempt reaching a new verdict — it is the same finding
	// observed again. Leave EscalationGeneration exactly as the prior
	// escalation recorded it (never call nextEscalationGeneration here):
	// bumping it would misreport this no-op tick as another re-run-and-fail
	// cycle, the same misattribution #4173 fixes for rebase-pr's own
	// infra-fault path, reached here by a different route (#4174).
	state.NoopGuardRepark = true
	if err := a.park(ctx, pr, prior, renderRemediationComment(state)); err != nil {
		return true, failGatherPRAdapter(stderr, err)
	}
	gaggle := l.Gaggle()
	if gaggle == "" {
		gaggle = providerGaggle()
	}
	if err := markRemediationNoopParked(l, remediationNoopKey(gaggle, pr.Number)); err != nil {
		pf(stderr, "error: mark unchanged-digest PR #%d parked: %v\n", pr.Number, err)
		return true, 1
	}
	return true, writeNoWorkResult(stdout, stderr, fmt.Sprintf("PR #%d was visibly parked after %d unchanged-digest runs", pr.Number, record.Attempts))
}

// writeGatherPRContextResult preserves the versioned brief contract after a
// provider-specific transport has obtained the verdict comments.
func writeGatherPRContextResult(
	selected providers.PullRequestSummary,
	behind bool,
	verdict *apiv1.Verdict,
	rawComments []providers.Comment,
	stdout, stderr io.Writer,
) int {
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

// gatherPRContextCandidateScope applies the durable handoff and run-target
// constraints before a provider prepares the candidate set. The provider paths
// deliberately retain their native preparation work after this point: GitHub
// resolves CI, escalation, and sibling state; ADO has only its label tier.
func gatherPRContextCandidateScope(
	root string,
	target remediationTarget,
	prs []providers.PullRequestSummary,
	stdout, stderr io.Writer,
) ([]providers.PullRequestSummary, bool, bool, int) {
	handoffNumber := providerInput("selectedNumber", "")
	claimedNumber, hasExistingClaim, err := claimedPullRequestNumber(root)
	if err != nil {
		pf(stderr, "error: resolve this run's existing PR claim: %v\n", err)
		return nil, false, true, 1
	}
	hasPinnedCandidate := hasExistingClaim
	if handoffNumber != "" {
		selectedNumber, parseErr := strconv.Atoi(handoffNumber)
		if parseErr != nil || selectedNumber <= 0 {
			pf(stderr, "error: selectedNumber input %q must be a positive integer\n", handoffNumber)
			return nil, false, true, 1
		}
		if hasExistingClaim && claimedNumber != selectedNumber {
			pf(stderr, "error: selectedNumber input PR #%d does not match this run's claimed PR #%d\n", selectedNumber, claimedNumber)
			return nil, false, true, 1
		}
		claimedNumber = selectedNumber
		hasPinnedCandidate = true
	}
	// #3985: a targeted run may only ever remediate the PR its trigger named.
	// update-behind-pr restricts its own selection to that PR, so a pinned
	// handoff or claim naming a different PR means the run's own upstream
	// state disagrees with the operator's argument — refuse rather than
	// remediate a pull request nobody asked for.
	if target.targeted && hasPinnedCandidate && claimedNumber != target.number {
		pf(stderr, "error: this run targets PR #%d but is pinned to PR #%d; refusing to remediate a pull request the trigger did not name\n",
			target.number, claimedNumber)
		return nil, false, true, 1
	}
	if !hasPinnedCandidate {
		return prs, false, false, 0
	}
	for _, pr := range prs {
		if pr.Number == claimedNumber {
			return []providers.PullRequestSummary{pr}, true, false, 0
		}
	}
	return nil, true, true, writeNoWorkResult(
		stdout, stderr, fmt.Sprintf("this run's claimed PR #%d is no longer open", claimedNumber),
	)
}

type gatherPRContextCandidateSelection struct {
	root                       string
	repo                       providers.RepositoryRef
	base                       string
	headPrefix                 string
	target                     remediationTarget
	listed                     []providers.PullRequestSummary
	eligible                   []providers.PullRequestSummary
	blockedDependents          map[int]int
	hasPinnedCandidate         bool
	supportsBehindBaseFallback bool
	behindBase                 func(providers.PullRequestSummary) (bool, error)
}

// gatherPRContextSelectCandidate applies the shared candidate ranking, target
// narrowing, and claim protocol after the provider-specific eligibility pass.
func gatherPRContextSelectCandidate(
	selection gatherPRContextCandidateSelection,
	stdout, stderr io.Writer,
) (providers.PullRequestSummary, bool, int) {
	candidates := selection.eligible
	var refusal string
	if !selection.hasPinnedCandidate {
		filtered := selection.eligible
		unclaimed, err := stageClaimAvailablePullRequests(
			selection.root,
			selection.repo,
			os.Getenv(executor.RunIDEnvVar),
			selection.eligible,
			time.Now(),
		)
		if err != nil {
			return providers.PullRequestSummary{}, true, failProviderStage(
				stderr, "filter claimed remediation candidates", err, remediationBriefResultFile,
			)
		}
		if selection.supportsBehindBaseFallback {
			candidates, _, err = selectRemediationCandidates(
				unclaimed, selection.blockedDependents, selection.behindBase,
			)
			if err != nil {
				pf(stderr, "error: determine remediation eligibility: %v\n", err)
				return providers.PullRequestSummary{}, true, 1
			}
		} else {
			// No ADO sibling-election evidence exists, so it cannot supply the
			// behind-base fallback tier.
			candidates, _ = strongRemediationCandidates(unclaimed)
		}
		// #3985: eligibility and tier ranking ran over the whole open-PR set,
		// exactly as on a scheduled tick; a targeted run then keeps only the
		// PR the trigger named, or ends here naming the filter that dropped it.
		candidates, refusal = selection.target.apply(
			remediationTargetStage{prs: selection.listed, reason: remediationTargetUnlistedReason(selection.base, selection.headPrefix)},
			remediationTargetStage{prs: filtered, reason: remediationTargetFilteredReason},
			remediationTargetStage{prs: unclaimed, reason: remediationTargetClaimedReason},
			remediationTargetStage{prs: candidates, reason: remediationTargetIneligibleReason},
		)
	} else {
		// The pinned candidate is already this run's target (cross-checked
		// above), so the only way it can vanish here is a remediation
		// exclusion — report that rather than the generic no-work reason.
		candidates, refusal = selection.target.apply(
			remediationTargetStage{prs: candidates, reason: remediationTargetFilteredReason},
		)
	}
	if refusal != "" {
		return providers.PullRequestSummary{}, true, writeNoWorkResult(stdout, stderr, refusal)
	}
	if len(candidates) == 0 {
		return providers.PullRequestSummary{}, true, writeNoWorkResult(stdout, stderr, "no PR needs remediation this cycle")
	}

	claimed, err := claimEligiblePullRequestInOrder(selection.root, selection.repo, candidates)
	if err != nil {
		pf(stderr, "error: claim eligible PR: %v\n", err)
		return providers.PullRequestSummary{}, true, 1
	}
	if claimed == nil {
		return providers.PullRequestSummary{}, true, writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}
	return *claimed, false, 0
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
// Only when no PR clears either strong tier does a lander merely behind its
// base become eligible, and then only if it is either crowned or alone. A crown
// is materialized by at least one live parked dependent naming the PR as a
// blocker. This keeps the rest of an overlapping wave parked until its
// predecessor lands instead of eagerly rebasing every behind-base sibling after
// each merge. Checking "behind base" requires fetching candidate branches, so
// behindBase is invoked only for those two cases when nothing stronger exists.
//
// #4163: the crown requirement alone made this path unreachable on the shipped
// default. The quota argument behind it is entirely about sibling ordering — an
// uncrowned PR in a wave will go behind again as soon as its predecessor lands,
// so updating it now buys nothing — and that argument says nothing at all about
// a lane holding one PR. `implementation` ships maxConcurrentRuns: 1, so such a
// lane never accumulates dependents, blockedDependents is 0 for the only PR
// there is on every tick, and behind-base could not fire even once. A solitary
// candidate has no predecessor to wait for: updating it once is the whole cost,
// not the first installment of many.
func selectRemediationCandidates(prs []providers.PullRequestSummary, blockedDependents map[int]int, behindBase func(providers.PullRequestSummary) (bool, error)) ([]providers.PullRequestSummary, remediationPriority, error) {
	candidates, best := strongRemediationCandidates(prs)
	if len(candidates) > 0 {
		return candidates, best, nil
	}

	solitary := len(prs) == 1
	for _, pr := range prs {
		// Same rationale as remediationPriorityFor: escalation exclusion
		// already happened upstream (self-heal-aware), so no re-check here.
		if blockedDependents[pr.Number] == 0 && !solitary {
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

	self := workspaceGitCommand(dir, "rev-parse", "--show-toplevel")
	selfOut, err := workspaceGitOutput(self)
	if err != nil {
		return held // not a git worktree (or unreadable): nothing to guard against
	}
	selfPath := canonicalPath(strings.TrimSpace(string(selfOut)))

	list := workspaceGitCommand(dir, "worktree", "list", "--porcelain")
	out, err := workspaceGitOutput(list)
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
	checkout := workspaceGitCommand(dir, "checkout", "-B", branch, "FETCH_HEAD")
	if out, err := workspaceGitCombinedOutput(checkout); err != nil {
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
	fetch := workspaceGitAuthEnvCommand(dir, env, "fetch", url, "refs/heads/"+branch)
	if out, err := workspaceGitCombinedOutput(fetch); err != nil {
		return "", fmt.Errorf("fetch %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	rev := workspaceGitCommand(dir, "rev-parse", "FETCH_HEAD")
	out, err := workspaceGitOutput(rev)
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
	cmd := workspaceGitCommand(dir, "merge-base", "--is-ancestor", baseSHA, headSHA)
	err := runWorkspaceGit(cmd)
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", baseSHA, headSHA, err)
}
