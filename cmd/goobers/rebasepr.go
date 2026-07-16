package main

import (
	"context"
	"encoding/json"
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

// runRebasePR implements `goobers rebase-pr` (issue #363): pr-remediation's
// rebase-first, finding-driven routing (design doc §5 D3). Routing is never
// rebase-driven: a clean rebase never suppresses a known substantive
// finding, and a rebase conflict is itself substantive.
//
//	rebase result | finding or failing CI? | action
//	clean         | no                     | force-with-lease push, clear label, done
//	clean         | yes                    | needs the agentic chain (not yet wired, see pr-remediation.yaml)
//	conflict      | either                 | needs the agentic chain (the conflict IS substantive)
//
// Re-checks out the PR's own branch first (checkoutExistingBranch, shared
// with gather-pr-context): this stage gets its OWN fresh worktree — see
// checkoutExistingBranch's doc comment — so it cannot assume gather-pr-
// context's checkout survived.
func runRebasePR(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rebase-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers rebase-pr [path]\n\n"+
			"Check out the selected PR's branch, attempt a rebase onto its base\n"+
			"(force-with-lease is mandatory for the eventual push — never a bare\n"+
			"push), and route on the result: a clean rebase with no substantive\n"+
			"finding or failing CI force-pushes and clears goobers:needs-remediation;\n"+
			"anything else (a conflict, substantive finding, or failing CI) needs the\n"+
			"agentic remediation chain, reported via the needsAgent output for the\n"+
			"workflow to route on. Requires selectedNumber/head/base\n"+
			"(Task.InputsFrom gather-pr-context's own outputs) and\n"+
			"hasSubstantiveFindings/hasFailingCI. Exit codes: 0 = routed, 1 =\n"+
			"business error, 2 = usage/IO error.\n")
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

	selectedNumber := providerInput("selectedNumber", "")
	head := providerInput("head", "")
	base := providerInput("base", "main")
	if selectedNumber == "" || head == "" {
		pf(stderr, "error: selectedNumber and head are required (inputsFrom gather-pr-context's own outputs)\n")
		return 1
	}
	hasSubstantiveFindings := providerInput("hasSubstantiveFindings", "false") == "true"
	hasFailingCI := providerInput("hasFailingCI", "false") == "true"

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
	if _, err := providerToken(capability.GitHubIssuesWrite); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	preRebaseSHA, err := checkoutExistingBranch(".", head, pushToken)
	if err != nil {
		pf(stderr, "error: checkout PR #%s's branch %q: %v\n", selectedNumber, head, err)
		return 1
	}

	conflict, err := attemptRebase(".", base, pushToken)
	if err != nil {
		pf(stderr, "error: rebase PR #%s onto %q: %v\n", selectedNumber, base, err)
		return 1
	}

	needsAgent := conflict || hasSubstantiveFindings || hasFailingCI
	resultFile := providerInput("resultFile", "rebase-result.json")

	if !conflict && !hasSubstantiveFindings {
		if err := forcePushWithLease(".", head, preRebaseSHA, pushToken); err != nil {
			pf(stderr, "error: force-push rebased PR #%s branch %q: %v\n", selectedNumber, head, err)
			return 1
		}
	}

	if !needsAgent {
		issuesToken, err := providerToken(capability.GitHubIssuesWrite)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		provider := newGitHubProvider(issuesToken)
		if _, err := provider.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
			Repository: repo, ID: selectedNumber, RemoveLabels: []string{needsRemediationLabel},
		}); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("clear %s from PR #%s", needsRemediationLabel, selectedNumber), err, "rebase-result.json")
		}
		if err := writeRebaseResult(resultFile, selectedNumber, head, false, false); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		pf(stdout, "PR #%s: clean rebase onto %s, no substantive finding — force-pushed and cleared %s\n", selectedNumber, base, needsRemediationLabel)
		return 0
	}

	if err := writeRebaseResult(resultFile, selectedNumber, head, conflict, needsAgent); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "PR #%s needs agentic remediation (conflict=%v, substantiveFindings=%v, failingCI=%v) — routing to remediation checkpoint\n", selectedNumber, conflict, hasSubstantiveFindings, hasFailingCI)
	return 0
}

// writeRebaseResult echoes selectedNumber/head forward alongside this
// stage's own needsAgent/conflict outcome — Task.InputsFrom resolves
// against the immediately preceding TASK's own Outputs (a gate never
// updates that chain; the gate this stage feeds is proof: apply-verdict's
// own doc comment establishes the same convention), so remediation-
// checkpoint (after rebase-gate) can only read selectedNumber/head if THIS
// stage re-emits them, exactly like gather-sibling-context re-emits
// pr-select's selectedNumber for apply-verdict two hops later.
func writeRebaseResult(resultFile, selectedNumber, head string, conflict, needsAgent bool) error {
	data, err := json.Marshal(map[string]string{
		"selectedNumber": selectedNumber,
		"head":           head,
		"needsAgent":     strconv.FormatBool(needsAgent),
		"conflict":       strconv.FormatBool(conflict),
	})
	if err != nil {
		return fmt.Errorf("marshal rebase result: %w", err)
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", resultFile, err)
	}
	return nil
}

// attemptRebase fetches base from origin and rebases the checked-out branch
// onto it, aborting cleanly on conflict — this stage NEVER leaves the
// worktree mid-rebase (design doc §5 D3: a conflict is itself a substantive
// finding, resolved by the agentic chain redoing the rebase in ITS OWN fresh
// worktree, not by inheriting this one's broken state, which per-stage
// worktree isolation wouldn't carry forward anyway). Distinguishes "the
// rebase conflicted" from "some other git failure" by whether `git rebase
// --abort` finds a rebase to abort at all: if abort succeeds, the original
// failure put us mid-rebase (a conflict); if abort itself fails, the
// original failure wasn't a rebase-in-progress state, so surface it
// directly rather than mask it as a false-negative "clean" result.
func attemptRebase(dir, base, token string) (conflict bool, err error) {
	url, err := originURL(dir)
	if err != nil {
		return false, err
	}
	auth := gitAuthEnv(token)
	fetch := exec.Command("git", "fetch", url, "refs/heads/"+base)
	fetch.Dir = dir
	fetch.Env = auth
	if out, err := fetch.CombinedOutput(); err != nil {
		return false, fmt.Errorf("fetch base %s: %w: %s", base, err, strings.TrimSpace(string(out)))
	}

	rebase := exec.Command("git", "rebase", "FETCH_HEAD")
	rebase.Dir = dir
	out, rerr := rebase.CombinedOutput()
	if rerr == nil {
		return false, nil
	}

	abort := exec.Command("git", "rebase", "--abort")
	abort.Dir = dir
	if aerr := abort.Run(); aerr != nil {
		return false, fmt.Errorf("git rebase FETCH_HEAD: %w: %s", rerr, strings.TrimSpace(string(out)))
	}
	return true, nil
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
