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

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

const (
	pushRemediatedResultName      = "push-remediated-result.json"
	pushRemediatedPublishedOutput = "published"
	pushRemediatedLocalHeadOutput = "localHead"
)

// runPushRemediated implements `goobers push-remediated` (issue #392):
// pr-remediation's publish step, the counterpart to implementation's
// push-branch (#237) for a workflow that re-enters on an EXISTING PR instead
// of opening a new one.
//
// It runs at the tail of the agentic chain — after implement committed its
// rework, the reviewer gate passed it, and local-ci proved it builds — and
// does the two things that actually close a remediation cycle: force-push the
// reworked branch to the PR's own head, and clear
// goobers:needs-remediation so merge-review picks the PR back up next cycle
// (design doc §5's "→ re-push → clear label").
//
// Why it re-derives its own context instead of taking it as inputs: it is the
// only stage here that CANNOT be threaded any. Task.InputsFrom resolves
// against the immediately preceding TASK's Outputs, and by the time this stage
// runs, `implement` (status + summary only) and `local-ci` (`make ci`) have
// each been that upstream in turn. So the PR number comes from the run's own
// durable claim (claimedPullRequestNumber), and the force-with-lease
// expectation comes from the head SHA remediation-checkpoint recorded on the
// PR's sticky state comment earlier in this same run.
//
// The lease expectation is deliberately NOT re-resolved from the remote here.
// forcePushWithLease's own doc comment explains why at length: re-resolving
// immediately before pushing makes the lease tautological — it would always
// match whatever just landed on the remote, silently defeating the "refuse if
// something pushed since I started" guarantee. A missing recorded SHA
// therefore fails the stage rather than falling back to a bare force-push:
// clobbering a human's concurrent push is exactly the outcome the lease exists
// to prevent, and this workflow's whole premise (§5) is that Goobers loses
// gracefully and re-selects next tick.
const pushRemediatedHelp = "Usage: goobers push-remediated [path]\n\n" +
	"Force-push (with lease) the remediated branch to the claimed PR's head\n" +
	"and clear goobers:needs-remediation so merge-review re-evaluates it.\n" +
	"Recovers the PR from this run's own claim ledger entry and the lease\n" +
	"expectation from the head SHA remediation-checkpoint recorded on the\n" +
	"PR's sticky state comment — neither can be threaded here, since the\n" +
	"agentic chain sits between this stage and the one that selected the PR.\n" +
	"Exit codes: 0 = pushed (or an idempotent no-op), 1 = business error,\n" +
	"2 = usage/IO error.\n"

func runPushRemediated(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("push-remediated", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "push-remediated")
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
	if repo.Provider == providers.ProviderADO {
		return runPushRemediatedADO(root, repo, stdout, stderr)
	}
	pushToken, err := providerToken(capability.RepoPush)
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
	prProvider, err := remediationStageProvider(root, repo, prToken, true)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	issuesProvider, err := remediationStageProvider(root, repo, issuesToken, true)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	transport := issueCommentPushTransport{prProvider: prProvider, issuesProvider: issuesProvider, repo: repo}
	return runPushRemediatedCore(root, repo, pushToken, transport, stdout, stderr)
}

// runPushRemediatedADO runs the push-remediated stage on Azure DevOps. The
// publish itself — recovering the run's own PR claim, the "branch unchanged ⇒
// nothing to publish" guard, and the mandatory force-with-lease — is
// provider-neutral and shared verbatim with the GitHub path
// (claimedPullRequestNumber, resolveHead, forcePushWithLease,
// writePushRemediatedResult, skipTerminalRemediatedPullRequest). Two reads and
// one write differ on ADO, all because *ADOProvider cannot satisfy
// remediationProvider (remediation-wiring-plan §0.1/§3.6):
//
//   - The PR summary (state/head branch) comes from the native ADO GetPullRequest
//     (a PollPullRequest adapter, §2.4).
//   - The pre-remediation head-SHA lease expectation is read from the sticky
//     remediation-state comment on the PR *thread* (ListPullRequestThreadComments,
//     §2.3) — ADO has no PR-issue-comment transport, so the sticky state rides a
//     thread; latestRemediationState parses that comment body unchanged.
//   - The needs-remediation label clear routes to the native PR-label DELETE
//     (RemovePullRequestLabel, §2.6) instead of UpdateWorkItem(ID: PR#), which on
//     ADO would mutate the unrelated work item sharing the PR's numeric id (the
//     wrong-object hazard, §0.5). Clearing the label is the re-entry trigger that
//     lets merge-review re-select the reworked PR.
//
// The provider is built from config-sourced ADO auth via the shared stage factory
// (no github:* token is resolved); only the provider-neutral repo:push credential
// feeds the force-push.
func runPushRemediatedADO(root string, repo providers.RepositoryRef, stdout, stderr io.Writer) int {
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

	transport := threadCommentPushTransport{provider: provider, repo: repo}
	return runPushRemediatedCore(root, repo, pushToken, transport, stdout, stderr)
}

// remediatedPushTransport isolates the two provider-native PR channels used by
// the otherwise provider-neutral publication decision: where state is read and
// how the re-entry label is cleared.
type remediatedPushTransport interface {
	GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error)
	ListStateComments(context.Context, string) ([]providers.Comment, error)
	ClearNeedsRemediation(context.Context, string) error
}

type issueCommentPushTransport struct {
	prProvider     remediationProvider
	issuesProvider remediationProvider
	repo           providers.RepositoryRef
}

func (t issueCommentPushTransport) GetPullRequest(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error) {
	return t.prProvider.GetPullRequest(ctx, repo, pullID)
}

func (t issueCommentPushTransport) ListStateComments(ctx context.Context, pullID string) ([]providers.Comment, error) {
	return t.prProvider.ListComments(ctx, t.repo, pullID)
}

func (t issueCommentPushTransport) ClearNeedsRemediation(ctx context.Context, pullID string) error {
	_, err := t.issuesProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: t.repo, ID: pullID, RemoveLabels: []string{needsRemediationLabel},
	})
	return err
}

type threadCommentPushTransport struct {
	provider *providers.ADOProvider
	repo     providers.RepositoryRef
}

func (t threadCommentPushTransport) GetPullRequest(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error) {
	return t.provider.GetPullRequest(ctx, repo, pullID)
}

func (t threadCommentPushTransport) ListStateComments(ctx context.Context, pullID string) ([]providers.Comment, error) {
	return t.provider.ListPullRequestThreadComments(ctx, t.repo, pullID)
}

func (t threadCommentPushTransport) ClearNeedsRemediation(ctx context.Context, pullID string) error {
	return t.provider.RemovePullRequestLabel(ctx, t.repo, pullID, needsRemediationLabel)
}

func runPushRemediatedCore(
	root string,
	repo providers.RepositoryRef,
	pushToken string,
	transport remediatedPushTransport,
	stdout, stderr io.Writer,
) int {
	selectedNumber, ok, err := claimedPullRequestNumber(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if !ok {
		// Deliberately NOT issue-close-out's released-claim no-op (#241). That
		// inference is sound there because close-out releases the claim itself
		// as its last step, so an absent entry really does mean "already done".
		// pr-remediation never releases mid-run — the claim is dropped only by
		// finalizeTerminalRun (which cannot precede this stage in a live run)
		// or by `goobers up`'s RecoverExpired sweep reaping an expired lease.
		// So reaching here means the lease expired mid-cycle, and the rework is
		// sitting committed-but-unpushed with the label still set.
		//
		// There is no way to tell which PR to publish to without the claim, so
		// this fails rather than reporting success: a green run whose message
		// says the work was published, when it was silently dropped, is the
		// worse outcome. The next cycle re-selects the PR and redoes the work.
		pf(stderr, "error: run holds no PR claim — its lease expired mid-cycle, so the remediated branch cannot be published; "+
			"the PR keeps %s and will be re-selected next cycle\n", needsRemediationLabel)
		return 1
	}

	pullID := strconv.Itoa(selectedNumber)
	ctx, cancel := providerCommandContext()
	defer cancel()
	current, err := transport.GetPullRequest(ctx, repo, strconv.Itoa(selectedNumber))
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("get pull request #%d", selectedNumber), err, "")
	}
	if current.State != "open" || current.Merged {
		return skipTerminalRemediatedPullRequest(selectedNumber, stdout, stderr)
	}

	rawComments, err := transport.ListStateComments(ctx, pullID)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("list comments on PR #%d", selectedNumber), err, "")
	}
	state, _, found := latestRemediationState(rawComments)
	if !found || state.HeadSHA == "" {
		pf(stderr, "error: PR #%d has no recorded pre-remediation head SHA to lease against "+
			"(remediation-checkpoint records it every cycle) — refusing to force-push without one\n", selectedNumber)
		return 1
	}

	// Nothing to publish is NOT success. If the branch still sits exactly where
	// it did before this cycle's agentic chain ran, the chain produced no
	// commit — an `implement` session that timed out or no-op'd, then a
	// reviewer that passed the PR's pre-existing diff (which, on a re-entered
	// branch, is never the empty diff #415's fast-fail keys on). Pushing would
	// be a no-op, but CLEARING the label would hand merge-review a PR it
	// already rejected, unchanged, as though it had been remediated. Leave the
	// label on and let the next cycle try again.
	localHead, err := resolveHead(".")
	if err != nil {
		pf(stderr, "error: resolve local head for PR #%d: %v\n", selectedNumber, err)
		return 1
	}
	if localHead == state.HeadSHA {
		pf(stderr, "error: PR #%d's branch is unchanged from its pre-remediation head %s — "+
			"the remediation produced no commit, so there is nothing to publish and %s stays set\n",
			selectedNumber, state.HeadSHA, needsRemediationLabel)
		return 1
	}

	current, err = transport.GetPullRequest(ctx, repo, strconv.Itoa(selectedNumber))
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("get pull request #%d", selectedNumber), err, "")
	}
	if current.State != "open" || current.Merged {
		return skipTerminalRemediatedPullRequest(selectedNumber, stdout, stderr)
	}

	if err := forcePushWithLease(".", current.Head, state.HeadSHA, pushToken); err != nil {
		return failProviderStage(
			stderr,
			fmt.Sprintf("force-push remediated PR #%d branch %q", selectedNumber, current.Head),
			err,
			pushRemediatedResultName,
		)
	}

	if err := transport.ClearNeedsRemediation(ctx, pullID); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("clear %s from PR #%d", needsRemediationLabel, selectedNumber), err, "")
	}

	pf(stdout, "PR #%d: pushed remediated branch %s and cleared %s\n", selectedNumber, current.Head, needsRemediationLabel)
	return writePushRemediatedResult(selectedNumber, true, current.Head, localHead, stderr)
}

func skipTerminalRemediatedPullRequest(selectedNumber int, stdout, stderr io.Writer) int {
	// The rework remains committed in the run journal, but publishing it to a
	// merged or closed PR branch would be actively wrong.
	pf(stdout, "PR #%d is no longer open (merged/closed during remediation) — nothing to push\n", selectedNumber)
	return writePushRemediatedResult(selectedNumber, false, "", "", stderr)
}

func writePushRemediatedResult(selectedNumber int, published bool, head, localHead string, stderr io.Writer) int {
	resultFile := providerInput("resultFile", pushRemediatedResultName)
	data, err := json.Marshal(map[string]string{
		"selectedNumber":              strconv.Itoa(selectedNumber),
		pushRemediatedPublishedOutput: strconv.FormatBool(published),
		"head":                        head,
		pushRemediatedLocalHeadOutput: localHead,
	})
	if err != nil {
		pf(stderr, "error: marshal push-remediated result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 2
	}
	return 0
}

// resolveHead returns dir's current HEAD commit SHA.
func resolveHead(dir string) (string, error) {
	cmd := workspaceGitCommand(dir, "rev-parse", "HEAD")
	out, err := workspaceGitOutput(cmd)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
