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

// confineDiffToDocsRoots enforces the docs-updater write-boundary (#1016) on the
// open-pr path: it lists every file this run's branch changes relative to base
// and refuses the cycle unless each is within one of the declared in-repo docs
// roots. Like confineDiffToConfigRoot it runs in the stage's worktree CWD and
// fails CLOSED — an inability to compute the diff refuses the PR, and an empty
// roots set is itself a refusal (configboundary.ErrNoDocsRoots).
func confineDiffToDocsRoots(base string, docsRoots []string) error {
	changed, err := changedFilesVsBase(base)
	if err != nil {
		return fmt.Errorf("compute changed files vs %q: %w", base, err)
	}
	return configboundary.ConfineToAny(docsRoots, changed)
}

// confineDiffToActionRoots enforces the Tutor's per-target-action-class write
// boundary (TUT-A5/#1217, docs/design/tutor-redesign.md §4.2): every file this
// run's branch changes relative to base must resolve into the SAME single
// declared action root — e.g. a run authoring a workflow-config change stays
// within "reference-workflows", a run authoring a new skill's body stays within
// "skills", but a single run may never do both at once. Unlike
// confineDiffToDocsRoots's ConfineToAny (a docs-updater diff may legitimately
// span several declared roots together), this refuses a diff that spans more
// than one action root even though every individual path is within some
// declared root (configboundary.ErrCrossRootAction). Fails CLOSED like every
// other write-boundary check here: an unverifiable diff, an empty roots set,
// or a cross-root diff all refuse the cycle rather than opening the PR.
func confineDiffToActionRoots(base string, actionRoots []string) error {
	changed, err := changedFilesVsBase(base)
	if err != nil {
		return fmt.Errorf("compute changed files vs %q: %w", base, err)
	}
	_, err = configboundary.ConfineExclusive(actionRoots, changed)
	return err
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
