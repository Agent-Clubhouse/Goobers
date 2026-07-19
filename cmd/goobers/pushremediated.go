package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
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
func runPushRemediated(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("push-remediated", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers push-remediated [path]\n\n"+
			"Force-push (with lease) the remediated branch to the claimed PR's head\n"+
			"and clear goobers:needs-remediation so merge-review re-evaluates it.\n"+
			"Recovers the PR from this run's own claim ledger entry and the lease\n"+
			"expectation from the head SHA remediation-checkpoint recorded on the\n"+
			"PR's sticky state comment — neither can be threaded here, since the\n"+
			"agentic chain sits between this stage and the one that selected the PR.\n"+
			"Exit codes: 0 = pushed (or an idempotent no-op), 1 = business error,\n"+
			"2 = usage/IO error.\n")
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
	if _, err := providerToken(capability.GitHubIssuesWrite); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(prToken)

	selectedNumber, ok, err := claimedPullRequestNumber(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if !ok {
		// Same resume-idempotency contract as issue-close-out's released-claim
		// no-op (#241): no claim means a prior attempt of this stage already
		// completed and released it. Re-running must not fail the run at its
		// final stage after all the real work succeeded.
		pln(stdout, "run holds no PR claim; a prior attempt already published this remediation — nothing to do")
		return 0
	}

	ctx := context.Background()
	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", "goobers/")
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
		// Merged or closed while this cycle's agentic chain was running. The
		// rework is not lost — it is committed on the run's branch and the
		// journal records it — but there is no longer an open PR to publish it
		// to, and force-pushing to a merged PR's branch would be actively
		// wrong. A clean no-op; next cycle selects on current facts.
		pf(stdout, "PR #%d is no longer open (merged/closed during remediation) — nothing to push\n", selectedNumber)
		return 0
	}

	rawComments, err := provider.ListComments(ctx, repo, strconv.Itoa(selectedNumber))
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("list comments on PR #%d", selectedNumber), err, "")
	}
	state, _, found := latestRemediationState(rawComments)
	if !found || state.HeadSHA == "" {
		pf(stderr, "error: PR #%d has no recorded pre-remediation head SHA to lease against "+
			"(remediation-checkpoint records it every cycle) — refusing to force-push without one\n", selectedNumber)
		return 1
	}

	if err := forcePushWithLease(".", current.Head, state.HeadSHA, pushToken); err != nil {
		pf(stderr, "error: force-push remediated PR #%d branch %q: %v\n", selectedNumber, current.Head, err)
		return 1
	}

	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: strconv.Itoa(selectedNumber), RemoveLabels: []string{needsRemediationLabel},
	}); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("clear %s from PR #%d", needsRemediationLabel, selectedNumber), err, "")
	}

	pf(stdout, "PR #%d: pushed remediated branch %s and cleared %s\n", selectedNumber, current.Head, needsRemediationLabel)
	return 0
}
