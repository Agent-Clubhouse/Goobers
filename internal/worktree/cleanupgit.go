package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/proc"
)

// cleanupGitTimeout bounds every git subprocess retained-worktree cleanup
// spawns — worktree remove/prune, worktree list (registration checks),
// merged-branch enumeration/ancestry checks, and branch deletion (#4325).
// Retained-worktree cleanup runs on daemon startup, the periodic sweep
// ticker, and shutdown; an external git process that can wait indefinitely
// holds all three hostage. The September 4 2026 MDB1 recovery hit exactly
// this on Windows with a large (~73,000 file) workcopy tree.
//
// A var, not a const, so tests can shrink it rather than waiting out the
// real timeout.
var cleanupGitTimeout = 2 * time.Minute

// cleanupKillWaitDelay bounds the secondary wait after killing a timed-out
// cleanup git subprocess's process tree. Mirrors
// cmd/goobers/validate.go's repositoryKillWaitDelay: a descendant may have
// escaped the tree while retaining an inherited output pipe, and Cmd.Wait
// cannot return until every holder of that pipe's write end exits — so this
// secondary wait, not the kill itself, is what actually bounds worst-case
// return time.
var cleanupKillWaitDelay = time.Second

// GitCleanupTimeoutError reports that a retained-worktree-cleanup git
// subprocess did not complete within cleanupGitTimeout. It never carries
// credential material: every git subcommand retained-worktree cleanup runs
// (worktree/branch/for-each-ref/merge-base) operates on the local mirror
// only, with no remote URL or token in its arguments, so Op and Path are
// always safe to surface to an operator.
type GitCleanupTimeoutError struct {
	// Op names the cleanup operation (e.g. "worktree remove"), not the raw
	// git argv, so the message reads as an operation an operator recognizes
	// rather than a git invocation they have to decode.
	Op      string
	Path    string
	Elapsed time.Duration
}

func (e *GitCleanupTimeoutError) Error() string {
	return fmt.Sprintf(
		"worktree cleanup: %s on %s did not complete within %s; a locked file, an antivirus scan, or a stuck network-drive-backed path are common causes on Windows — safe to retry once the underlying condition clears",
		e.Op, e.Path, e.Elapsed,
	)
}

// Timeout reports true so errors.Is(err, context.DeadlineExceeded)-style
// timeout classification code (and Go's own net.Error convention) can
// recognize this as a timeout without a type assertion.
func (e *GitCleanupTimeoutError) Timeout() bool { return true }

// runCleanupGit runs a retained-worktree-cleanup git subprocess bounded by
// cleanupGitTimeout. On expiry it kills the whole process tree (not just the
// direct child, via proc.Start/Tree.Kill) so a descendant that escaped the
// tree cannot keep Cmd.Wait blocked, then gives that wait at most
// cleanupKillWaitDelay more before giving up on it entirely — Wait may still
// be running in its own goroutine after runCleanupGit returns, but nothing
// in this package waits on it again.
//
// A caller-driven cancellation of the outer ctx (not our own timeout)
// returns ctx.Err() unchanged, so callers can still distinguish "the caller
// gave up" from "this one git subprocess timed out."
func runCleanupGit(ctx context.Context, dir, op string, args ...string) error {
	_, err := runCleanupGitOutput(ctx, dir, op, args...)
	return err
}

// runCleanupGitOutput is runCleanupGit's output-returning twin, for cleanup
// call sites that need git's stdout (e.g. `worktree list --porcelain`,
// `for-each-ref`) subject to the identical bound.
func runCleanupGitOutput(ctx context.Context, dir, op string, args ...string) (string, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, cleanupGitTimeout)
	defer cancel()

	cmd := exec.CommandContext(boundedCtx, "git", hardenedGitArgs(args)...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Separate buffers, not a shared one: os/exec copies stdout and stderr
	// on two independent goroutines, and bytes.Buffer is not safe for
	// concurrent writes from both at once (caught by -race in CI). The
	// error path below concatenates them after Wait has returned, once
	// there is no concurrent writer left.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	tree, err := proc.Start(cmd)
	if err != nil {
		return "", err
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err = <-waitDone:
	case <-boundedCtx.Done():
		_ = tree.Kill()
		select {
		case <-waitDone:
		case <-time.After(cleanupKillWaitDelay):
			// A descendant may have escaped the tree while retaining an
			// inherited output pipe (see cleanupKillWaitDelay's doc); do not
			// let cleanup block indefinitely on that leaked wait.
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &GitCleanupTimeoutError{Op: op, Path: dir, Elapsed: cleanupGitTimeout}
	}
	if err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		combined := append(append([]byte{}, stdout.Bytes()...), stderr.Bytes()...)
		return "", &gitCommandError{args: args, cause: err, output: combined, exitCode: exitCode}
	}
	return strings.TrimSpace(stdout.String()), nil
}
