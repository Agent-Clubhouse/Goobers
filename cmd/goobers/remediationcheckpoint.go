package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// DefaultRemediationBudget is D4's liberal per-PR repass-cycle budget
// (design doc §6): the number of pr-remediation cycles a PR may go through
// before remediation-checkpoint gives up and escalates rather than
// continuing to select it. Config-overridable via the --budget flag,
// mirroring gate.DefaultMaxRepasses/Evaluator.MaxRepasses's override at
// in-run altitude — lifted here to PR altitude (issue #364).
const DefaultRemediationBudget = 10

const remediationEscalatedLabel = "goobers:merge-escalated"

// remediationState is pr-remediation's OWN durable per-PR loop-control
// state (D4's cycle counter + D5's last diff digest) — distinct from
// merge-review's Verdict payload (applyverdict.go's verdict-json), since it
// is written and read by a different workflow's runs. Embedded in a sticky
// PR comment the same way, and for the same reason: gather-pr-context
// already established that a PR comment is the only durable cross-run
// channel available at this altitude (neither workflow shares a journal/
// runID with the other's runs, or across its own runs).
//
// It is ALSO the escalation-livelock breaker's (#716) self-heal snapshot: on
// an escalation, EscalatedHeadSHA/EscalatedBaseSHA record the PR's head/base
// at the moment escalation was recorded, so a later selection attempt
// (pr-select.go / gatherprcontext.go's escalationStillBlocks) can tell
// "genuinely still stuck" (current SHAs match) from "context changed since
// escalation — a sibling merge advanced base, or new commits landed" (SHAs
// differ), the latter re-enabling selection automatically without needing a
// human to clear the label.
type remediationState struct {
	// Cycles is the number of pr-remediation cycles recorded for this PR so
	// far (this checkpoint's own count — incremented once per run that
	// reaches this stage, regardless of what the earlier stages did).
	Cycles int `json:"cycles"`
	// LastDiffDigest is the content-addressed digest of the most recently
	// checkpointed cycle's `git diff base...HEAD` — compared against the
	// current cycle's digest to detect a no-progress repeat (#316's in-run
	// same-diff check, lifted to PR altitude per design doc §6 D5).
	LastDiffDigest string `json:"lastDiffDigest"`
	// Escalated marks this recorded state as an escalation event (goobers:
	// merge-escalated was applied) rather than an ordinary advancing cycle.
	Escalated bool `json:"escalated,omitempty"`
	// EscalatedReason is the human-readable cause (budget exhaustion or a
	// byte-identical repeat), carried so a later sticky-comment edit can
	// still render it without re-deriving it.
	EscalatedReason string `json:"escalatedReason,omitempty"`
	// EscalatedHeadSHA / EscalatedBaseSHA are the PR's head/base SHA at the
	// moment of escalation — the self-heal comparison snapshot (#716).
	EscalatedHeadSHA string `json:"escalatedHeadSha,omitempty"`
	EscalatedBaseSHA string `json:"escalatedBaseSha,omitempty"`
}

// remediationStatePattern matches the machine-readable payload
// remediationStateComment appends to its posted comment.
var remediationStatePattern = regexp.MustCompile(`(?s)<!-- remediation-state: (.*?) -->`)

// remediationStateComment marshals s into the HTML-comment payload a
// checkpoint run posts as a PR comment.
func remediationStateComment(s remediationState) (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal remediation-state payload: %w", err)
	}
	return fmt.Sprintf("<!-- remediation-state: %s -->", data), nil
}

// parseRemediationStateComment recovers the remediationState a prior
// checkpoint run embedded in a PR comment. Returns ok=false if body has no
// embedded payload — the normal "no checkpoint recorded yet" outcome for a
// PR's first pr-remediation cycle, not a parse error.
func parseRemediationStateComment(body string) (remediationState, bool) {
	m := remediationStatePattern.FindStringSubmatch(body)
	if m == nil {
		return remediationState{}, false
	}
	var s remediationState
	if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
		return remediationState{}, false
	}
	return s, true
}

// renderRemediationComment builds the full sticky-comment body for state: a
// human-readable prose line (distinct for the escalated vs. ordinary-cycle
// case) followed by the embedded machine payload parseRemediationStateComment
// reads back. Escalated prose describes what ACTUALLY happens now (#716
// design item 4) — the PR is parked, not (as the old text falsely claimed)
// permanently excluded from ever being looked at again.
func renderRemediationComment(state remediationState) string {
	var prose string
	if state.Escalated {
		prose = fmt.Sprintf(
			"**pr-remediation escalated**\n\n%s. Parked until this PR's head or base changes, or a human removes `%s`.",
			state.EscalatedReason, remediationEscalatedLabel,
		)
	} else {
		prose = fmt.Sprintf("pr-remediation checkpoint: cycle %d, diff digest `%s`.", state.Cycles, state.LastDiffDigest)
	}
	payload, err := remediationStateComment(state)
	if err != nil {
		// Marshaling a plain struct of strings/ints/bools does not fail in
		// practice; if it somehow did, the prose alone is still a useful
		// comment — just without the machine-readable tail this run's own
		// state would otherwise carry forward.
		return prose
	}
	return prose + "\n\n" + payload
}

// postOrUpdateStickyComment posts state as a new PR comment, or — when
// existingCommentID names a comment already found on the thread (the sticky
// remediation-state comment a prior cycle posted) — edits that comment in
// place instead (#716 AC3: at most one escalation comment per PR per digest;
// repeated escalations/cycles edit the sticky comment rather than growing a
// new one every run).
func postOrUpdateStickyComment(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, prNumber int, existingCommentID, body string) error {
	if existingCommentID != "" {
		return provider.UpdateComment(ctx, repo, existingCommentID, body)
	}
	_, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         strconv.Itoa(prNumber),
		Comment:    body,
	})
	return err
}

// escalationStillBlocks reports whether pr's CURRENT goobers:merge-escalated
// label still blocks it from selection by merge-review's pr-select or
// pr-remediation's gather-pr-context (#716's core fix). A PR not currently
// carrying the label is never blocked by this check (false, nil) — this is
// the ordinary case for the vast majority of candidates and costs nothing.
//
// A PR that DOES carry the label is only genuinely still stuck if its
// current head/base SHA match the snapshot remediation-checkpoint recorded
// at the moment it escalated: unchanged head AND unchanged base means
// nothing about the PR's situation has moved since a human or the agent last
// looked, so re-selecting it would just reproduce the same escalation. If
// EITHER SHA has moved — new commits pushed, or a sibling merge advanced the
// base (post-merge.go's fan-out sets goobers:needs-remediation on every
// sibling targeting the same base, which is exactly the trigger) — the PR's
// context has genuinely changed and selection re-enables automatically
// (AC2's self-heal), without needing a human to clear the label by hand.
//
// Fetches comments only for PRs that carry the label — a small, by-design
// subset once this fix is live — so this stays cheap for the common case of
// an unlabeled candidate.
func escalationStillBlocks(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	if !hasAnyLabel(pr.Labels, []string{remediationEscalatedLabel}) {
		return false, nil
	}
	rawComments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return false, err
	}
	state, _, found := latestRemediationState(rawComments)
	if !found || !state.Escalated {
		// Labeled but no recorded escalation snapshot — a PR escalated
		// before this fix shipped, or a human applied the label by hand.
		// Fail closed: still blocks until a human clears the label, since
		// there is no snapshot to compare against.
		return true, nil
	}
	if state.EscalatedHeadSHA != pr.HeadSHA || state.EscalatedBaseSHA != pr.BaseSHA {
		return false, nil
	}
	return true, nil
}

// latestRemediationState scans comments (oldest first, ListComments' own
// order) for the LAST one carrying an embedded remediation-state payload —
// only the most recently recorded cycle/escalation is still actionable —
// and also returns that comment's ID, so a caller can edit it in place
// (postOrUpdateStickyComment) rather than posting a new one. found is false
// if no comment in the thread carries a payload (the PR's first
// pr-remediation cycle), not an error.
func latestRemediationState(comments []providers.Comment) (state remediationState, commentID string, found bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if s, ok := parseRemediationStateComment(comments[i].Body); ok {
			return s, comments[i].ID, true
		}
	}
	return remediationState{}, "", false
}

// runRemediationCheckpoint implements `goobers remediation-checkpoint`
// (issue #364): lifts the in-run repass budget (gate.DefaultMaxRepasses,
// internal/gate/evaluate.go's Evaluator) and same-diff escalation (#316,
// LastDiffDigest) to PR altitude (design doc §6 D4/D5). Meant to run as
// pr-remediation's last stage each cycle, immediately after whichever
// stage(s) push the remediated branch (#363) — it re-checks-out the PR's
// own branch itself (this stage gets its own fresh worktree; an earlier
// stage's checkout does not survive to here, same reason gather-pr-context
// and rebase-pr each do their own), reads the PR's most recently recorded
// cycle count + diff digest back from a sticky PR comment, compares this
// cycle's actual diff against it, and either escalates
// (goobers:merge-escalated, clearing needs-remediation so the machine stops
// selecting it) on budget exhaustion or a byte-identical repeat, or records
// the advanced state for next cycle.
func runRemediationCheckpoint(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("remediation-checkpoint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers remediation-checkpoint [--budget N] [path]\n\n"+
			"Re-checkout the PR's own branch (this stage gets its own fresh\n"+
			"worktree), read pr-remediation's durable per-PR cycle counter + last\n"+
			"diff digest back from a sticky PR comment, compare this cycle's\n"+
			"actual diff (git diff base...HEAD) against it, and either\n"+
			"escalate (goobers:merge-escalated, clearing needs-remediation) on\n"+
			"budget exhaustion or a byte-identical repeat, or record the advanced\n"+
			"state as a new sticky comment. Requires selectedNumber (inputsFrom\n"+
			"gather-pr-context's selectedNumber output). Exit codes: 0 = checkpoint\n"+
			"recorded (escalated or not — both are normal outcomes), 1 = business\n"+
			"error, 2 = usage/IO error.\n")
	}
	budget := fs.Int("budget", DefaultRemediationBudget, "liberal per-PR repass-cycle budget before escalating (D4)")
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
		pf(stderr, "error: selectedNumber is required (inputsFrom gather-pr-context's selectedNumber output)\n")
		return 1
	}
	selectedNumber, err := strconv.Atoi(selectedNumberStr)
	if err != nil {
		pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
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
	// repo:push is not for writing here — it's the same credential
	// checkoutExistingBranch uses to fetch the PR's branch (see the
	// re-checkout comment below).
	pushToken, err := providerToken(capability.RepoPush)
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
		pln(stdout, "PR is no longer open (merged/closed since selection) — checkpoint moot, nothing to record")
		return 0
	}

	// Re-checkout the PR's own branch: this stage gets its OWN fresh
	// worktree (internal/runner's buildEnvelope keys worktree continuity on
	// the run's shared branch, not on whatever an earlier stage locally
	// checked out — #133/#363's own re-checkout for the same reason), so
	// gather-pr-context's or rebase-pr's checkout does not survive to here.
	// Without this, diffDigest would diff against whatever this fresh
	// worktree defaulted to (the run's own untouched base checkout), not
	// the PR's actual just-pushed content.
	if _, err := checkoutExistingBranch(".", current.Head, pushToken); err != nil {
		pf(stderr, "error: checkout PR #%d's branch %q: %v\n", selectedNumber, current.Head, err)
		return 1
	}

	digest, err := diffDigest(".", current.BaseSHA)
	if err != nil {
		pf(stderr, "error: compute diff digest for PR #%d: %v\n", selectedNumber, err)
		return 1
	}

	rawComments, err := provider.ListComments(ctx, repo, strconv.Itoa(selectedNumber))
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("list comments on PR #%d", selectedNumber), err, "")
	}
	// Latest comment carrying an embedded payload wins, same rationale as
	// gather-pr-context's verdict scan: only the most recently recorded
	// checkpoint state is still actionable. Its comment ID (if any) is the
	// sticky comment this cycle edits in place (#716 AC3), rather than
	// posting a new one.
	prior, priorCommentID, _ := latestRemediationState(rawComments)

	sameDiff := prior.LastDiffDigest != "" && prior.LastDiffDigest == digest
	cycles := prior.Cycles + 1
	if *budget <= 0 {
		*budget = DefaultRemediationBudget
	}
	exceeded := cycles > *budget

	var state remediationState
	if exceeded || sameDiff {
		reason := fmt.Sprintf("repass budget exhausted (%d/%d cycles)", cycles, *budget)
		if sameDiff {
			reason = fmt.Sprintf("this cycle's diff is byte-identical to the immediately prior cycle's (digest %s) — an unchanged diff cannot make progress", digest)
		}
		state = remediationState{
			Cycles: cycles, LastDiffDigest: digest,
			Escalated: true, EscalatedReason: reason,
			EscalatedHeadSHA: current.HeadSHA, EscalatedBaseSHA: current.BaseSHA,
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository:   repo,
			ID:           strconv.Itoa(selectedNumber),
			AddLabels:    []string{remediationEscalatedLabel},
			RemoveLabels: []string{needsRemediationLabel},
		}); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("escalate PR #%d", selectedNumber), err, "")
		}
		if err := postOrUpdateStickyComment(ctx, provider, repo, selectedNumber, priorCommentID, renderRemediationComment(state)); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("record escalation comment on PR #%d", selectedNumber), err, "")
		}
		pf(stdout, "escalated PR #%d: %s\n", selectedNumber, reason)
		return 0
	}

	state = remediationState{Cycles: cycles, LastDiffDigest: digest}
	if err := postOrUpdateStickyComment(ctx, provider, repo, selectedNumber, priorCommentID, renderRemediationComment(state)); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("record checkpoint state on PR #%d", selectedNumber), err, "")
	}

	pf(stdout, "recorded checkpoint for PR #%d: cycle %d/%d, digest %s\n", selectedNumber, cycles, *budget, digest)
	return 0
}

// diffDigest returns the hex-encoded sha256 digest of `git diff
// baseSHA...HEAD` at dir — the same content-addressing idea
// internal/worktree.Worktree.Diff + internal/runner's recordReviewerDiff use
// for the in-run same-diff check (#316), computed directly here since
// pr-remediation's stages (like gather-pr-context before it,
// checkoutExistingBranch/isBehindBase) shell out to git directly rather
// than going through the runner-internal Worktree type.
func diffDigest(dir, baseSHA string) (string, error) {
	if baseSHA == "" {
		return "", fmt.Errorf("PR has no recorded base SHA")
	}
	cmd := exec.Command("git", "diff", baseSHA+"...HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git diff %s...HEAD: %w: %s", baseSHA, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git diff %s...HEAD: %w", baseSHA, err)
	}
	sum := sha256.Sum256(out)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
