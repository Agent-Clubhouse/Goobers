package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// completeParentMaterialization is only for a durable, unfinished creation at
// its verified archive HEAD. Validate through a private HEAD index before any
// mutation: existing files/index entries may match HEAD or be absent, but may
// never contain edits. A normal surviving dirty checkout does not use this path.
func (m *Manager) completeParentMaterialization(ctx context.Context, path, head, repoURL string) error {
	env := recoveryMirrorEnvironment()
	if err := runGitWithEnv(ctx, path, env, "diff", "--cached", "--quiet", "--no-ext-diff", "--no-renames", "--diff-filter=ACMRTUXB", head, "--"); err != nil {
		return fmt.Errorf("parent creation has unexpected staged state: %w", err)
	}
	dir, err := os.MkdirTemp("", "goobers-parent-checkout-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	probe := append(append([]string(nil), env...), "GIT_INDEX_FILE="+filepath.Join(dir, "index"))
	if err := runGitWithEnv(ctx, path, probe, "read-tree", head); err != nil {
		return err
	}
	missing, err := parentMaterializationMissing(ctx, path, probe)
	if err != nil {
		return err
	}
	// The existing index was proven to contain only a subset of HEAD. Fill
	// that baseline, then create only missing files without --force. If a file
	// appears after validation, checkout refuses to overwrite it.
	if err := runGitWithEnv(ctx, path, env, "read-tree", head); err != nil {
		return err
	}
	partial := m.partialClone && mirrorIsPartial(ctx, m.repoDirForKey(repoKey(repoURL)))
	for len(missing) > 0 {
		count := min(len(missing), 64)
		args := append([]string{"checkout-index", "--"}, missing[:count]...)
		if partial {
			err = m.runRemoteGit(ctx, repoURL, path, args...)
		} else {
			err = runGitWithEnv(ctx, path, env, args...)
		}
		if err != nil {
			return err
		}
		missing = missing[count:]
	}
	status, err := rawGitOutput(ctx, path, env, "status", "--porcelain=v1", "--untracked-files=all", "-z")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return errors.New("parent creation did not reach a clean archive baseline")
	}
	return nil
}

func parentMaterializationMissing(ctx context.Context, path string, env []string) ([]string, error) {
	if err := runGitWithEnv(ctx, path, env, "diff", "--quiet", "--no-ext-diff", "--no-renames", "--diff-filter=ACMRTUXB", "--"); err != nil {
		return nil, fmt.Errorf("parent creation has unexpected working changes: %w", err)
	}
	other, err := rawGitOutput(ctx, path, env, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	if len(other) != 0 {
		return nil, errors.New("parent creation has unexpected untracked files")
	}
	data, err := rawGitOutput(ctx, path, env, "ls-files", "--deleted", "-z")
	if err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
		return nil, errors.New("parent creation exceeds missing-path budget")
	}
	var result []string
	for _, name := range bytes.Split(data, []byte{0}) {
		if len(name) != 0 {
			result = append(result, string(name))
		}
	}
	return result, nil
}
