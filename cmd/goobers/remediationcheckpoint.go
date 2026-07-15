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

// runRemediationCheckpoint implements `goobers remediation-checkpoint`
// (issue #364): lifts the in-run repass budget (gate.DefaultMaxRepasses,
// internal/gate/evaluate.go's Evaluator) and same-diff escalation (#316,
// LastDiffDigest) to PR altitude (design doc §6 D4/D5). Meant to run as
// pr-remediation's last stage each cycle, immediately after whichever
// stage(s) push the remediated branch (#363) — it reads the PR's most
// recently recorded cycle count + diff digest back from a sticky PR
// comment, compares this cycle's actual diff against it, and either
// escalates (goobers:merge-escalated, clearing needs-remediation so the
// machine stops selecting it) on budget exhaustion or a byte-identical
// repeat, or records the advanced state for next cycle.
func runRemediationCheckpoint(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("remediation-checkpoint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers remediation-checkpoint [--budget N] [path]\n\n"+
			"Read pr-remediation's durable per-PR cycle counter + last diff digest\n"+
			"back from a sticky PR comment, compare this cycle's actual diff (git\n"+
			"diff base...HEAD in the current worktree) against it, and either\n"+
			"escalate (goobers:merge-escalated, clearing needs-remediation) on\n"+
			"budget exhaustion or a byte-identical repeat, or record the advanced\n"+
			"state as a new sticky comment. Checks out the PR's own branch first\n"+
			"(this stage gets its own fresh worktree, same as every other\n"+
			"pr-remediation stage), so the diff it computes is always the PR's\n"+
			"actual current diff, not whatever the worktree defaulted to. Requires\n"+
			"selectedNumber and head (inputsFrom the preceding task's own\n"+
			"outputs). Exit codes: 0 = checkpoint recorded (escalated or not —\n"+
			"both are normal outcomes), 1 = business error, 2 = usage/IO error.\n")
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
	head := providerInput("head", "")
	if selectedNumberStr == "" || head == "" {
		pf(stderr, "error: selectedNumber and head are required (inputsFrom the preceding task's own outputs)\n")
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
	if _, err := providerToken(capability.GitHubIssuesWrite); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
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
		pf(stderr, "error: list pull requests: %v\n", err)
		return 1
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

	// This stage gets its own fresh worktree, defaulting to the runner's
	// own run branch, not the PR's actual branch (the same per-stage
	// worktree isolation gather-pr-context/rebase-pr re-check-out for
	// themselves) — without this, diffDigest below would compute a
	// meaningless diff against whatever the worktree happened to default
	// to, silently corrupting D5's same-diff check.
	if _, err := checkoutExistingBranch(".", head, pushToken); err != nil {
		pf(stderr, "error: checkout PR #%d's branch %q: %v\n", selectedNumber, head, err)
		return 1
	}

	digest, err := diffDigest(".", current.BaseSHA)
	if err != nil {
		pf(stderr, "error: compute diff digest for PR #%d: %v\n", selectedNumber, err)
		return 1
	}

	rawComments, err := provider.ListComments(ctx, repo, strconv.Itoa(selectedNumber))
	if err != nil {
		pf(stderr, "error: list comments on PR #%d: %v\n", selectedNumber, err)
		return 1
	}
	// Latest comment carrying an embedded payload wins, same rationale as
	// gather-pr-context's verdict scan: only the most recently recorded
	// checkpoint state is still actionable.
	var prior remediationState
	for i := len(rawComments) - 1; i >= 0; i-- {
		if s, ok := parseRemediationStateComment(rawComments[i].Body); ok {
			prior = s
			break
		}
	}

	sameDiff := prior.LastDiffDigest != "" && prior.LastDiffDigest == digest
	cycles := prior.Cycles + 1
	if *budget <= 0 {
		*budget = DefaultRemediationBudget
	}
	exceeded := cycles > *budget

	if exceeded || sameDiff {
		reason := fmt.Sprintf("repass budget exhausted (%d/%d cycles)", cycles, *budget)
		if sameDiff {
			reason = fmt.Sprintf("this cycle's diff is byte-identical to the immediately prior cycle's (digest %s) — an unchanged diff cannot make progress", digest)
		}
		comment := fmt.Sprintf("**pr-remediation escalated**\n\n%s. A human must look — this PR is no longer selected by pr-remediation.", reason)
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository:   repo,
			ID:           strconv.Itoa(selectedNumber),
			AddLabels:    []string{remediationEscalatedLabel},
			RemoveLabels: []string{needsRemediationLabel},
			Comment:      comment,
		}); err != nil {
			pf(stderr, "error: escalate PR #%d: %v\n", selectedNumber, err)
			return 1
		}
		pf(stdout, "escalated PR #%d: %s\n", selectedNumber, reason)
		return 0
	}

	stateComment, err := remediationStateComment(remediationState{Cycles: cycles, LastDiffDigest: digest})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         strconv.Itoa(selectedNumber),
		Comment:    stateComment,
	}); err != nil {
		pf(stderr, "error: record checkpoint state on PR #%d: %v\n", selectedNumber, err)
		return 1
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
