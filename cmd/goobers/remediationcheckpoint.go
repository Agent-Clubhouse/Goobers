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
	"os"
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
	// HeadSHA / BaseSHA are the PR's head/base SHA at the moment THIS cycle
	// was recorded (every cycle, not only escalations) — the input the next
	// cycle's rebase-aware same-diff check reads back (#832). A byte-identical
	// LastDiffDigest only means "no progress" when the base has ALSO not moved
	// since: a clean rebase onto newer main legitimately reproduces the same
	// base...HEAD diff while advancing BaseSHA, which is progress toward
	// mergeability, not a stall. Empty on records written before #832 shipped,
	// in which case the check falls back to the digest-only behavior.
	HeadSHA string `json:"headSha,omitempty"`
	BaseSHA string `json:"baseSha,omitempty"`
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
	// --escalate is the reviewer-verdict=fail path (design doc §4 D2: "a
	// fundamentally wrong approach is not burned on remediation budget"), not
	// a loop-control outcome: escalate unconditionally with the caller's
	// reason, skipping the budget and same-diff checks entirely. Issue #392.
	escalateReason := fs.String("escalate", "", "escalate unconditionally with this reason, skipping the D4/D5 checks")
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

	var selectedNumber int
	if selectedNumberStr := providerInput("selectedNumber", ""); selectedNumberStr != "" {
		n, err := strconv.Atoi(selectedNumberStr)
		if err != nil {
			pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
			return 1
		}
		selectedNumber = n
	} else {
		// Ledger fallback (#392): in --escalate mode this stage runs after the
		// agentic chain, where Task.InputsFrom can no longer reach
		// gather-pr-context's selectedNumber — implement and local-ci each
		// became the upstream in turn. The run's own PR claim is the durable
		// answer. Still an error if there is no claim either: escalating
		// without knowing WHICH PR would label an arbitrary one.
		n, ok, err := claimedPullRequestNumber(root)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		if !ok {
			pf(stderr, "error: selectedNumber is required (inputsFrom gather-pr-context's selectedNumber output, or a PR claim held by this run)\n")
			return 1
		}
		selectedNumber = n
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
		// Halt, don't continue: there is no longer a PR to remediate, so
		// spending an agentic session on it would be pure waste (#392).
		if err := writeCheckpointResult(stderr, false, selectedNumber, "", ""); err != nil {
			return 1
		}
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

	stalled := remediationStalled(prior, digest, current.BaseSHA)
	cycles := prior.Cycles + 1
	if *budget <= 0 {
		*budget = DefaultRemediationBudget
	}
	exceeded := cycles > *budget
	forced := *escalateReason != ""

	var state remediationState
	if exceeded || stalled || forced {
		reason := fmt.Sprintf("repass budget exhausted (%d/%d cycles)", cycles, *budget)
		if stalled {
			reason = fmt.Sprintf("this cycle's diff is byte-identical to the immediately prior cycle's on the same base (digest %s) — an unchanged diff on an unchanged base cannot make progress", digest)
		}
		// Checked last so an explicit caller-supplied reason always wins the
		// prose, even on a cycle that also happens to be stalled or over
		// budget — the reviewer's terminal verdict is the more specific and
		// more actionable cause to show a human.
		if forced {
			reason = *escalateReason
		}
		state = remediationState{
			Cycles: cycles, LastDiffDigest: digest,
			HeadSHA: current.HeadSHA, BaseSHA: current.BaseSHA,
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
		if err := writeCheckpointResult(stderr, false, selectedNumber, current.Head, current.HeadSHA); err != nil {
			return 1
		}
		pf(stdout, "escalated PR #%d: %s\n", selectedNumber, reason)
		return 0
	}

	state = remediationState{
		Cycles: cycles, LastDiffDigest: digest,
		HeadSHA: current.HeadSHA, BaseSHA: current.BaseSHA,
	}
	if err := postOrUpdateStickyComment(ctx, provider, repo, selectedNumber, priorCommentID, renderRemediationComment(state)); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("record checkpoint state on PR #%d", selectedNumber), err, "")
	}
	if err := writeCheckpointResult(stderr, true, selectedNumber, current.Head, current.HeadSHA); err != nil {
		return 1
	}

	pf(stdout, "recorded checkpoint for PR #%d: cycle %d/%d, digest %s\n", selectedNumber, cycles, *budget, digest)
	return 0
}

// writeCheckpointResult emits this stage's routing output (issue #392).
//
// continueRemediation is what checkpoint-gate branches on: "true" means this
// cycle may spend the agentic chain on the PR, "false" means it must not —
// because the checkpoint escalated it (budget exhausted, a no-progress
// repeat, or a caller-forced reviewer "fail"), or because the PR is no
// longer open. It is a "true"/"false" STRING for the same reason
// gather-pr-context stringifies its own booleans: only string-valued
// top-level result-file keys survive into a downstream stage's GOOBERS_INPUT_*
// env var.
//
// selectedNumber/head/headSha are echoed forward because a gate never updates
// Task.InputsFrom's upstream-Outputs chain (rebase-pr's writeRebaseResult doc
// establishes the convention) — push-remediated sits two hops past
// checkpoint-gate, so anything it needs from here must be re-emitted here.
// headSha in particular is the PR's remote tip BEFORE this cycle pushes
// anything, which is exactly the non-tautological --force-with-lease
// expectation push-remediated requires.
//
// A resultFile is only written when the stage declares one; a caller running
// this command outside a workflow (or a workflow that does not route on the
// outcome) is unaffected.
func writeCheckpointResult(stderr io.Writer, continueRemediation bool, selectedNumber int, head, headSHA string) error {
	resultFile := providerInput("resultFile", "")
	if resultFile == "" {
		return nil
	}
	data, err := json.Marshal(map[string]string{
		"continueRemediation": strconv.FormatBool(continueRemediation),
		"selectedNumber":      strconv.Itoa(selectedNumber),
		"head":                head,
		"headSha":             headSHA,
	})
	if err != nil {
		pf(stderr, "error: marshal checkpoint result: %v\n", err)
		return err
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return err
	}
	return nil
}

// remediationStalled reports whether this cycle is a genuine no-progress
// repeat that should trip the same-diff escalation (design doc §6 D5): the
// cycle's diff is byte-identical to the prior recorded cycle's AND the base
// has not advanced since.
//
// The base clause is #832's fix. A clean rebase onto newer main legitimately
// reproduces the same `base...HEAD` diff while advancing BaseSHA — that is
// what a clean rebase IS — and being current with main is progress toward
// mergeability, not a stall, so a byte-identical diff after a base advance
// must NOT escalate. Only an identical diff on the SAME base is genuinely
// stuck. Uses base rather than head deliberately: an identical-content
// re-push advances the head SHA without making any progress, so head
// movement alone must not suppress escalation. When prior.BaseSHA is empty
// (a state recorded before #832 shipped, or the PR's first cycle), the base
// clause is inert and behavior falls back to the original digest-only check.
func remediationStalled(prior remediationState, digest, currentBaseSHA string) bool {
	sameDiff := prior.LastDiffDigest != "" && prior.LastDiffDigest == digest
	rebasedSincePrior := prior.BaseSHA != "" && prior.BaseSHA != currentBaseSHA
	return sameDiff && !rebasedSincePrior
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
