package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Bound path-list storage separately from archive bytes. Exceeding this bound
// refuses capture; it never silently drops files and authorizes cleanup.
const maxSnapshotPathBytes = 8 << 20

// Select files in the original repository, where .git/info/exclude and local
// core.excludesFile are authoritative. Capture uses private Git metadata to
// suppress conversions, so asking that private directory to select untracked
// files would lose these ignore rules and could archive private local outputs.
func snapshotPaths(ctx context.Context, repository, directory string) (string, error) {
	path := filepath.Join(directory, "capture-paths")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	var selected bytes.Buffer
	writer := &archiveBudgetWriter{destination: &selected, remaining: maxSnapshotPathBytes}
	if err := recoveryGit(ctx, repository, writer, "ls-files", "--cached", "--others", "--exclude-standard", "--deduplicate", "-z"); err != nil {
		return "", fmt.Errorf("select recovery snapshot paths: %w", err)
	}
	if err := writeExistingSnapshotPaths(repository, selected.Bytes(), file); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func writeExistingSnapshotPaths(repository string, data []byte, destination io.Writer) error {
	for len(data) > 0 {
		name, rest, complete := bytes.Cut(data, []byte{0})
		if !complete || !filepath.IsLocal(string(name)) {
			return fmt.Errorf("invalid recovery snapshot path list")
		}
		data = rest
		info, err := os.Lstat(filepath.Join(repository, string(name)))
		if errors.Is(err, os.ErrNotExist) {
			// The preceding add --update already records tracked deletions.
			// Re-adding a now-absent path would fail Git's pathspec check.
			continue
		} else if err != nil {
			return err
		}
		if info.IsDir() {
			if _, err := os.Lstat(filepath.Join(repository, string(name), ".git")); err == nil {
				return ErrNestedRecoveryRequired
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if _, err := destination.Write(append(name[:len(name):len(name)], 0)); err != nil {
			return err
		}
	}
	return nil
}
