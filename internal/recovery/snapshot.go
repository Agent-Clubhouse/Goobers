package recovery

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// CaptureSnapshot records the current tracked and non-ignored untracked files
// in a commit without changing HEAD, the working files, or the caller's index.
// The caller must own the worktree exclusively for this operation. Ignored
// build outputs are not added; existing tracked files remain tracked. Tracked
// files the source checkout reports as clean keep their canonical index blobs,
// so a checkout's line-ending or encoding conversion is not recorded as an
// edit; changed tracked files and untracked files are captured as raw bytes. The
// returned object is not durable recovery until pinned and archived. Captured
// content is not permission to restore reserved runtime assets: the restoration
// coordinator must enforce those protections before applying a retained patch.
// identityTime must be durable and stable across retries (for example, the
// owning run's recorded start time), not a fresh wall-clock reading per attempt.
// Actual archive capture/retention times are recorded separately in Record.
func CaptureSnapshot(ctx context.Context, repository, runID string, identityTime time.Time) (string, error) {
	if _, err := RefForRun(runID); err != nil {
		return "", err
	}
	if identityTime.IsZero() {
		return "", fmt.Errorf("snapshot requires a stable identity timestamp")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := snapshotRoot(ctx, repository)
	if err != nil {
		return "", err
	}
	repository = root
	var head boundedRefOutput
	if err := recoveryGit(ctx, repository, &head, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return "", fmt.Errorf("read recovery snapshot parent: %w", err)
	}
	parent := strings.TrimSpace(head.String())
	if !gitObjectID.MatchString(parent) {
		return "", fmt.Errorf("invalid recovery snapshot parent")
	}
	directory, err := privateGitDirectory(ctx, repository, "goobers-recovery-index-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	environment, err := snapshotEnvironment(ctx, repository, directory, parent)
	if err != nil {
		return "", err
	}
	paths, err := snapshotPaths(ctx, repository, directory)
	if err != nil {
		return "", err
	}
	updates, err := snapshotTrackedUpdates(ctx, repository, directory)
	if err != nil {
		return "", err
	}
	if err := snapshotIndex(ctx, repository, environment); err != nil {
		return "", err
	}
	commands, err := snapshotAddCommands(updates, paths)
	if err != nil {
		return "", err
	}
	for _, args := range commands {
		if err := recoveryGitWithEnv(ctx, repository, io.Discard, environment, args...); err != nil {
			return "", fmt.Errorf("capture recovery index: %w", err)
		}
	}
	var tree boundedRefOutput
	if err := recoveryGitWithEnv(ctx, repository, &tree, environment, "write-tree"); err != nil {
		return "", fmt.Errorf("write recovery snapshot tree: %w", err)
	}
	treeID := strings.TrimSpace(tree.String())
	if !gitObjectID.MatchString(treeID) {
		return "", fmt.Errorf("invalid recovery snapshot tree")
	}
	if err := requireSelfContainedSnapshot(ctx, repository, treeID); err != nil {
		return "", err
	}
	var snapshot boundedRefOutput
	date := identityTime.UTC().Format(time.RFC3339)
	// Git's author/committer dates retain only whole seconds. Bind the full
	// durable capture window in the message too, or two distinct windows can
	// share a snapshot ref while their immutable retention metadata conflicts.
	message := "Retain recovery state for " + runID + "\n\nCapture identity: " + identityTime.UTC().Format(time.RFC3339Nano)
	environment = append(environment,
		"GIT_AUTHOR_NAME=Goobers Recovery", "GIT_AUTHOR_EMAIL=recovery@goobers.invalid",
		"GIT_COMMITTER_NAME=Goobers Recovery", "GIT_COMMITTER_EMAIL=recovery@goobers.invalid",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if err := recoveryGitWithEnv(ctx, repository, &snapshot, environment,
		"-c", "commit.gpgsign=false", "commit-tree", treeID, "-p", parent, "-m", message); err != nil {
		return "", fmt.Errorf("write recovery snapshot commit: %w", err)
	}
	id := strings.TrimSpace(snapshot.String())
	if !gitObjectID.MatchString(id) {
		return "", fmt.Errorf("invalid recovery snapshot commit")
	}
	return id, nil
}

// snapshotAddCommands stages the selected tracked updates and untracked paths.
// An empty pathspec file makes `add` operate on the whole worktree: for
// --update that would re-add Git-clean tracked files raw (#6917), and for
// forced --all it would capture ignored build outputs. Only add selected paths.
func snapshotAddCommands(updates, untracked string) ([][]string, error) {
	var commands [][]string
	for _, step := range []struct {
		file string
		args []string
	}{
		{updates, []string{"--literal-pathspecs", "add", "--update"}},
		{untracked, []string{"--literal-pathspecs", "add", "--all", "--force", "--sparse"}},
	} {
		selected, err := os.Stat(step.file)
		if err != nil {
			return nil, fmt.Errorf("inspect recovery snapshot paths: %w", err)
		}
		if selected.Size() > 0 {
			commands = append(commands, append(step.args, "--pathspec-file-nul", "--pathspec-from-file="+step.file))
		}
	}
	return commands, nil
}
