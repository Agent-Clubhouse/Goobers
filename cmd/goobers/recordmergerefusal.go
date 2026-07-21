package main

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// defaultDemotionThreshold is how many consecutive merge refusals at an
// unchanged head demote a crowned lander (#950). A lander that refuses to merge
// this many times at the SAME head is genuinely stuck — its head is not
// advancing (a remediation push would move it) yet it cannot land — so
// re-crowning it every cycle only re-deadlocks its cluster. Overridable via the
// demotionThreshold stage input.
const defaultDemotionThreshold = 3

const recordMergeRefusalHelp = "Usage: goobers record-merge-refusal [path]\n\n" +
	"Record a merge-pr refusal against the selected PR's durable demotion\n" +
	"counter (#950). After a bounded number of refusals at an unchanged\n" +
	"head, apply goobers:merge-demoted so the election crowns a sibling\n" +
	"instead and the cluster drains around the stuck PR. Runs on\n" +
	"merge-gate's fail branch. Exit codes: 0 = recorded (or nothing to\n" +
	"do), 1 = business error, 2 = usage/IO error.\n"

// runRecordMergeRefusal implements `goobers record-merge-refusal` (#950): the
// stage merge-gate routes to on its fail branch. It records a durable per-PR
// counter of consecutive merge refusals at an unchanged head and, once that
// crosses the threshold, applies goobers:merge-demoted so the election stops
// re-crowning a lander that cannot merge and its blocked cluster drains around
// it. The demotion self-heals the moment the PR's head advances (a new commit),
// so a fixed PR is never permanently sidelined.
func runRecordMergeRefusal(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("record-merge-refusal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "record-merge-refusal")
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

	selectedNumberStr := providerInput("selectedNumber", "")
	if selectedNumberStr == "" {
		pf(stderr, "error: selectedNumber is required (inputsFrom merge-pr's selectedNumber)\n")
		return 1
	}
	selectedNumber, err := strconv.Atoi(selectedNumberStr)
	if err != nil {
		pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
		return 1
	}
	reason := providerInput("reason", "")
	// The head the refusal happened at, echoed by merge-pr (its own headSha
	// SHA-pin input) — the demotion is keyed by it so a refusal at a NEW head is
	// a fresh attempt, and it is the snapshot demotionStillHolds/post-merge use
	// to self-heal the demotion when the head later advances.
	headSha := providerInput("selectedHeadSha", "")
	if headSha == "" {
		pf(stderr, "error: selectedHeadSha is required (inputsFrom merge-pr's selectedHeadSha)\n")
		return 1
	}
	threshold := defaultDemotionThreshold
	if t := providerInput("demotionThreshold", ""); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			threshold = n
		}
	}

	// Advisory-mode "refusals" are not real merge attempts — the stage evaluates
	// without ever trying to land — so they must never accrue toward demotion,
	// else under advisory mode every cycle would demote every lander.
	if strings.Contains(reason, "advisory mode") {
		pf(stdout, "PR #%d: advisory-mode result, not a real merge refusal — not recording demotion\n", selectedNumber)
		return 0
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
	provider := newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))
	ctx, cancel := providerCommandContext()
	defer cancel()

	comments, err := provider.ListComments(ctx, repo, selectedNumberStr)
	if err != nil {
		return failProviderStage(stderr, "list comments", err, "")
	}
	prior, priorCommentID, found := latestMergeDemotionState(comments)

	attempts := 1
	if found && prior.HeadSHA == headSha {
		// Same head as the last recorded refusal — genuinely stuck; accumulate.
		attempts = prior.Attempts + 1
	}
	demoted := attempts >= threshold

	state := mergeDemotionState{
		Attempts:   attempts,
		Demoted:    demoted,
		HeadSHA:    headSha,
		Reason:     reason,
		RecordedAt: time.Now().UTC(),
	}

	// Label bookkeeping. Apply merge-demoted at/over threshold. If the PR was
	// demoted at an OLDER head and has since advanced (attempts reset to 1),
	// clear the label here as belt-and-suspenders with post-merge's self-heal
	// sweep — the head moved, so it is a fresh attempt and no longer demoted.
	var update providers.UpdateWorkItemRequest
	update.Repository = repo
	update.ID = selectedNumberStr
	switch {
	case demoted:
		// Also route the stuck lander to pr-remediation: its merge blocker is
		// usually a base advance intersecting its files or newly-red CI, both of
		// which remediation resolves by rebasing/reworking — and that advances
		// the PR's head, which is exactly what self-heals the demotion. Without
		// this a base-blocked lander would sit demoted with no path to move its
		// head, so it could never satisfy "not permanently demoted".
		update.AddLabels = []string{mergeDemotedLabel, needsRemediationLabel}
	case found && prior.Demoted && prior.HeadSHA != headSha:
		update.RemoveLabels = []string{mergeDemotedLabel}
	}
	if len(update.AddLabels) > 0 || len(update.RemoveLabels) > 0 {
		if _, err := provider.UpdateWorkItem(ctx, update); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("update merge-demoted label on PR #%d", selectedNumber), err, "")
		}
	}

	if err := postOrUpdateStickyComment(ctx, provider, repo, selectedNumber, priorCommentID, renderMergeDemotionComment(state, threshold)); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("record demotion comment on PR #%d", selectedNumber), err, "")
	}

	if demoted {
		pf(stdout, "PR #%d demoted after %d merge refusal(s) at head %s — election will crown a sibling (#950)\n", selectedNumber, attempts, headSha)
	} else {
		pf(stdout, "PR #%d: recorded merge refusal %d/%d at head %s (#950)\n", selectedNumber, attempts, threshold, headSha)
	}
	return 0
}

// renderMergeDemotionComment builds the sticky comment body: a human-readable
// summary plus the embedded machine-readable payload the election read-sites
// and post-merge self-heal parse back out.
func renderMergeDemotionComment(s mergeDemotionState, threshold int) string {
	payload, _ := mergeDemotionComment(s)
	var msg string
	if s.Demoted {
		msg = fmt.Sprintf(
			"⚠️ **Merge demoted** (#950). This pull request could not merge on %d consecutive attempt(s) at the same head `%s`, so `goobers:merge-demoted` is applied: the lander election will crown a sibling instead, letting the cluster drain around this PR while it is worked separately (pr-remediation still runs on its own merits). It self-heals automatically the moment a new commit advances the head. Most recent refusal: %s",
			s.Attempts, s.HeadSHA, s.Reason)
	} else {
		msg = fmt.Sprintf(
			"Merge refusal recorded (#950): attempt %d of %d at head `%s`. One more refusal at an unchanged head demotes this PR so its blocked cluster can drain around it. Reason: %s",
			s.Attempts, threshold, s.HeadSHA, s.Reason)
	}
	return msg + "\n\n" + payload
}
