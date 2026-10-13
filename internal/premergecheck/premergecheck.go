// Package premergecheck verifies, immediately before the managed merge path
// lands a pull request, that the pull request's head still builds once merged
// onto the CURRENT tip of its base branch (issue #7132).
//
// A pull request's CI result describes the base its last run used. When
// another pull request lands first, the two can each be green alone and still
// break the base once combined (one removes a symbol the other still uses).
// Re-running the full CI suite before every merge is expensive; a fast compile
// check of the actual merge result is not, and it closes exactly that gap.
package premergecheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Outcome classifies what a check concluded about the merge result.
type Outcome string

// Outcomes Run can report. Each is a measurement, never an infrastructure
// failure: a check that could not be carried out returns an error instead, so
// an unmeasured merge is never mistaken for a passing one.
const (
	// OutcomePassed means every command succeeded on the merge result.
	OutcomePassed Outcome = "passed"
	// OutcomeConflict means the head does not merge cleanly onto the base tip.
	OutcomeConflict Outcome = "merge-conflict"
	// OutcomeFailed means a command failed on the merge result.
	OutcomeFailed Outcome = "failed"
	// OutcomeHeadMoved means the head ref no longer points at the pinned
	// commit, so nothing was measured; the verdict is stale.
	OutcomeHeadMoved Outcome = "head-moved"
)

// maxOutputTail bounds the command output a failed check keeps, from the tail,
// where compilers and vet report the error that matters.
const maxOutputTail = 2048

// GitFunc builds a git command to run in dir. The caller owns authentication
// and environment hardening; Run only supplies the arguments.
type GitFunc func(ctx context.Context, dir string, args ...string) *exec.Cmd

// Request describes one pre-merge check.
type Request struct {
	// RepoDir is any git checkout of the target repository. Run adds and then
	// removes a disposable worktree beside it; the checkout's own tree is
	// never modified.
	RepoDir string
	// Remote is the URL (or remote name) the base and head branches are
	// fetched from.
	Remote string
	// BaseBranch is the branch the pull request targets.
	BaseBranch string
	// HeadRef is the ref the pull request's head is fetched from: a full ref
	// such as refs/pull/7/head (which also serves fork heads), or a bare
	// branch name taken as refs/heads/<name>.
	HeadRef string
	// HeadSHA is the pinned head commit. A fetched head that differs is
	// reported as OutcomeHeadMoved rather than measuring a commit nobody
	// reviewed.
	HeadSHA string
	// Commands are run in order on the merge result; the first failure stops.
	Commands [][]string
	// Env is the environment the commands run with.
	Env []string
	// Git builds every git invocation. Required.
	Git GitFunc
}

// Result reports a completed measurement.
type Result struct {
	Outcome Outcome
	// BaseTipSHA is the base branch tip the head was merged onto.
	BaseTipSHA string
	// HeadTipSHA is the commit the head ref resolved to.
	HeadTipSHA string
	// Command is the failing command (OutcomeFailed only).
	Command string
	// Output is the bounded tail of the failing command's combined output,
	// or of the merge's output on a conflict.
	Output string
}

// ParseCommands splits spec into one argv per non-blank line, on whitespace.
// Commands run without a shell so a check behaves identically on every host;
// lines starting with '#' are comments.
func ParseCommands(spec string) [][]string {
	var commands [][]string
	for _, line := range strings.Split(spec, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		commands = append(commands, strings.Fields(line))
	}
	return commands
}

// Run fetches the current base tip and the pinned head, merges them in a
// disposable worktree, and runs every command on the result.
func Run(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	baseTip, err := fetchTip(ctx, req, req.BaseBranch)
	if err != nil {
		return Result{}, err
	}
	headTip, err := fetchTip(ctx, req, req.HeadRef)
	if err != nil {
		return Result{}, err
	}
	if headTip != req.HeadSHA {
		return Result{Outcome: OutcomeHeadMoved, BaseTipSHA: baseTip, HeadTipSHA: headTip}, nil
	}

	scratch, err := os.MkdirTemp("", "goobers-premerge-*")
	if err != nil {
		return Result{}, fmt.Errorf("premergecheck: create worktree directory: %w", err)
	}
	dir := filepath.Join(scratch, "tree")
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		_, _ = combined(req.Git(cleanup, req.RepoDir, "worktree", "remove", "--force", dir))
		_ = os.RemoveAll(scratch)
		_, _ = combined(req.Git(cleanup, req.RepoDir, "worktree", "prune"))
	}()
	if out, err := combined(req.Git(ctx, req.RepoDir, "worktree", "add", "--detach", dir, baseTip)); err != nil {
		return Result{}, fmt.Errorf("premergecheck: add worktree at %s: %w: %s", baseTip, err, out)
	}
	return measure(ctx, req, dir, baseTip)
}

// measure merges the pinned head into the base tip checked out at dir and
// runs the commands on the result.
func measure(ctx context.Context, req Request, dir, baseTip string) (Result, error) {
	result := Result{BaseTipSHA: baseTip, HeadTipSHA: req.HeadSHA}
	conflicted, out, err := merge(ctx, req, dir)
	if err != nil {
		return Result{}, err
	}
	if conflicted {
		result.Outcome = OutcomeConflict
		result.Output = tail(out)
		return result, nil
	}
	for _, command := range req.Commands {
		out, err := runCommand(ctx, dir, req.Env, command)
		if err == nil {
			continue
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || ctx.Err() != nil {
			return Result{}, fmt.Errorf("premergecheck: run %q: %w", strings.Join(command, " "), err)
		}
		result.Outcome = OutcomeFailed
		result.Command = strings.Join(command, " ")
		result.Output = tail(out)
		return result, nil
	}
	result.Outcome = OutcomePassed
	return result, nil
}

func (req Request) validate() error {
	switch {
	case req.Git == nil:
		return errors.New("premergecheck: a git command builder is required")
	case req.RepoDir == "" || req.Remote == "":
		return errors.New("premergecheck: a repository checkout and remote are required")
	case req.BaseBranch == "" || req.HeadRef == "" || req.HeadSHA == "":
		return errors.New("premergecheck: base branch, head ref and head SHA are required")
	case len(req.Commands) == 0:
		return errors.New("premergecheck: at least one command is required")
	}
	return nil
}

func fetchTip(ctx context.Context, req Request, ref string) (string, error) {
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	if out, err := combined(req.Git(ctx, req.RepoDir, "fetch", "--no-tags", req.Remote, ref)); err != nil {
		return "", fmt.Errorf("premergecheck: fetch %q: %w: %s", ref, err, out)
	}
	out, err := req.Git(ctx, req.RepoDir, "rev-parse", "FETCH_HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("premergecheck: resolve fetched %q: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// merge merges the pinned head into the detached base tip at dir. A merge that
// stops on unmerged paths is a conflict; any other failure is an error.
func merge(ctx context.Context, req Request, dir string) (bool, string, error) {
	out, err := combined(req.Git(ctx, dir,
		"-c", "user.name=goobers", "-c", "user.email=goobers@localhost",
		"merge", "--no-ff", "--no-edit", req.HeadSHA))
	if err == nil {
		return false, "", nil
	}
	unmerged, uerr := req.Git(ctx, dir, "diff", "--name-only", "--diff-filter=U").Output()
	if uerr == nil && strings.TrimSpace(string(unmerged)) != "" {
		return true, out, nil
	}
	return false, "", fmt.Errorf("premergecheck: merge %s: %w: %s", req.HeadSHA, err, out)
}

func runCommand(ctx context.Context, dir string, env, command []string) (string, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func combined(cmd *exec.Cmd) (string, error) {
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func tail(output string) string {
	output = strings.TrimSpace(output)
	if len(output) <= maxOutputTail {
		return output
	}
	return "…" + output[len(output)-maxOutputTail:]
}
