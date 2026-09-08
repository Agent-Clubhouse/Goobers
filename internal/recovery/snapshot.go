package recovery

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// CaptureSnapshot records the current tracked and non-ignored untracked files
// in a commit without changing HEAD, the working files, or the caller's index.
// The caller must own the worktree exclusively for this operation. Ignored
// build outputs are not added; existing tracked files remain tracked. The
// returned object is not durable recovery until pinned and archived. Captured
// content is not permission to restore reserved runtime assets: the restoration
// coordinator must enforce those protections before applying a retained patch.
func CaptureSnapshot(ctx context.Context, repository, runID string) (string, error) {
	if _, err := RefForRun(runID); err != nil {
		return "", err
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
	directory, err := os.MkdirTemp("", "goobers-recovery-index-*")
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
	if err := snapshotIndex(ctx, repository, environment); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"add", "--update", "--", "."},
		{"--literal-pathspecs", "add", "--all", "--force", "--sparse", "--pathspec-file-nul", "--pathspec-from-file=" + paths},
	} {
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
	environment = append(environment,
		"GIT_AUTHOR_NAME=Goobers Recovery", "GIT_AUTHOR_EMAIL=recovery@goobers.invalid",
		"GIT_COMMITTER_NAME=Goobers Recovery", "GIT_COMMITTER_EMAIL=recovery@goobers.invalid")
	if err := recoveryGitWithEnv(ctx, repository, &snapshot, environment,
		"-c", "commit.gpgsign=false", "commit-tree", treeID, "-p", parent, "-m", "Retain recovery state for "+runID); err != nil {
		return "", fmt.Errorf("write recovery snapshot commit: %w", err)
	}
	id := strings.TrimSpace(snapshot.String())
	if !gitObjectID.MatchString(id) {
		return "", fmt.Errorf("invalid recovery snapshot commit")
	}
	return id, nil
}
