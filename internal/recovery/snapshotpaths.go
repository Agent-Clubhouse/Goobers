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
const maxSnapshotPathBytes = 64 << 20

var errSnapshotPathListTooLarge = errors.New("recovery snapshot path list exceeds byte budget")

type snapshotPathBudgetWriter struct {
	destination io.Writer
	remaining   int64
}

func (w *snapshotPathBudgetWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errSnapshotPathListTooLarge
	}
	n, err := w.destination.Write(data)
	w.remaining -= int64(n)
	return n, err
}

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
	writer := &snapshotPathBudgetWriter{destination: &selected, remaining: maxSnapshotPathBytes}
	if err := recoveryGit(ctx, repository, writer, "ls-files", "--others", "--exclude-standard", "-z"); err != nil {
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
			return fmt.Errorf("recovery snapshot path disappeared after selection: %q", name)
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
