package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/goobers/goobers/internal/configboundary"
)

// confineDiffToConfigRoot enforces the Tutor config write-boundary (#104/T4,
// #223) on the real open-pr path: it lists every file this run's branch changes
// relative to base and refuses the cycle if any is outside configRoot. It runs
// in the stage's CWD, which the runner sets to the run's worktree (checked out
// to the run branch with the prior stages' committed changes, #133), so the git
// diff is the run's actual proposed change.
//
// It fails CLOSED: an inability to compute the diff (a git error) refuses the
// PR rather than opening it unverified — when confinement is requested, an
// unverifiable diff is treated as a boundary breach.
func confineDiffToConfigRoot(base, configRoot string) error {
	changed, err := changedFilesVsBase(base)
	if err != nil {
		return fmt.Errorf("compute changed files vs %q: %w", base, err)
	}
	return configboundary.Confine(configRoot, changed)
}

// changedFilesVsBase returns the repo-relative paths this branch changes vs base
// (three-dot: the diff since the merge-base, i.e. the PR's file set).
// --no-renames so a file moved out of the config root surfaces as its new,
// out-of-root path rather than being hidden by rename detection.
func changedFilesVsBase(base string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--no-renames", "--name-only", base+"...HEAD")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git diff: %s: %w", strings.TrimSpace(string(ee.Stderr)), err)
		}
		return nil, fmt.Errorf("git diff: %w", err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}
