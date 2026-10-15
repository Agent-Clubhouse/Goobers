package worktree

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

// CreateChildReadOnlyView derives a detached view of the admitted fork. It uses
// only the existing imported snapshot, never the configured branch or remote.
// Repeated creation verifies exact existing ownership without resetting edits.
func (m *Manager) CreateChildReadOnlyView(ctx context.Context, child ChildOptions, stage string) (*Worktree, error) {
	opts, err := childReadOnlyOptions(child, stage)
	if err != nil {
		return nil, err
	}
	return m.createSnapshotWorkspace(ctx, opts)
}

// AdoptChildReadOnlyView recovers an existing view without creating or fetching.
// Missing, modified or substituted custody must be reconciled by its owner.
func (m *Manager) AdoptChildReadOnlyView(ctx context.Context, child ChildOptions, stage string) (*Worktree, error) {
	opts, err := childReadOnlyOptions(child, stage)
	if err != nil {
		return nil, err
	}
	return m.adoptSnapshotWorkspace(ctx, opts)
}

func childReadOnlyOptions(child ChildOptions, stage string) (CreateOptions, error) {
	opts, err := childCreateOptions(child)
	if err != nil {
		return opts, err
	}
	opts.RunID, err = ChildReadOnlyViewID(child.Gaggle, child.RunID, stage)
	if err != nil {
		return CreateOptions{}, err
	}
	opts.Branch, opts.RetainOnCleanup = "", false
	return opts, nil
}

// ChildReadOnlyViewID binds cleanup to the same immutable stage identity used
// to create its detached view, without provisioning or reading credentials.
func ChildReadOnlyViewID(gaggle, workspaceID, stage string) (string, error) {
	if !validRunID(workspaceID) || stage == "" || len(stage) > 256 || strings.ContainsAny(stage, "\x00\r\n") {
		return "", fmt.Errorf("child read-only view requires valid ownership and a bounded stage")
	}
	id := sha256.Sum256(fmt.Appendf(nil, "%q:%q:%q", gaggle, workspaceID, stage))
	return fmt.Sprintf("child-view-%x", id[:16]), nil
}

func verifyChildDetachedView(ctx context.Context, path, snapshot string) error {
	err := runGit(ctx, path, "symbolic-ref", "--quiet", "HEAD")
	var commandErr *gitCommandError
	if !errors.As(err, &commandErr) || commandErr.exitCode != 1 || strings.TrimSpace(string(commandErr.output)) != "" {
		return fmt.Errorf("child read-only view is not detached")
	}
	head, err := gitOutput(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || head != snapshot {
		return fmt.Errorf("child read-only view changed its pinned revision")
	}
	status, err := gitOutput(ctx, path, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return fmt.Errorf("child read-only view contains unexpected changes")
	}
	return nil
}
