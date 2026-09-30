package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ensureMirrorInvariants restores the configuration a managed mirror must
// have before this package fetches into it (#5422). A linked worktree shares
// the mirror's config, so a stage that runs `git init` inside its run
// worktree re-initializes the MIRROR as non-bare. The mirror's HEAD then
// counts as a checked-out branch and every later refresh fetch refuses to
// update it — one agent command failing every run in the gaggle until an
// operator repaired the file. The manager owns the mirror, so it re-asserts
// core.bare on every refresh. The check reads first and writes only on drift,
// so a healthy mirror's shared config is never rewritten.
func ensureMirrorInvariants(ctx context.Context, dir string) error {
	bare, err := gitOutput(ctx, dir, "config", "--bool", "--get", "core.bare")
	if err == nil && bare == "true" {
		return nil
	}
	if err := runGit(ctx, dir, "config", "core.bare", "true"); err != nil {
		return fmt.Errorf("worktree: restore core.bare in %s: %w", dir, err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "warning: worktree: restored core.bare=true on managed mirror %s (was %q); a run worktree re-initialized it\n", dir, bare)
	return nil
}

// isMaintenanceLockContention reports whether a failed maintenance pass lost
// only a lock another git process holds on the mirror: a concurrent `git gc`
// (an agent's auto-gc in a linked worktree, or another Manager in the same
// daemon, whose per-repository mutex this one cannot see) or any other
// repository lockfile. Both are benign and self-clearing — the holder is
// doing the same housekeeping — unlike a corrupt object store or a full disk,
// which must stay fatal.
func isMaintenanceLockContention(err error) bool {
	var gitErr *gitCommandError
	if !errors.As(err, &gitErr) {
		return false
	}
	message := strings.ToLower(string(gitErr.output))
	return strings.Contains(message, "gc is already running") ||
		strings.Contains(message, ".lock': file exists")
}
