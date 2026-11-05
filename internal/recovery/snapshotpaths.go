package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Bound path-list storage separately from archive bytes. Exceeding this bound
// refuses capture; it never silently drops files and authorizes cleanup.
const maxSnapshotPathBytes = 64 << 20

var errSnapshotPathListTooLarge = errors.New("recovery snapshot path list exceeds byte budget")

const maxSnapshotExclusionBytes = 16 << 10

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
func snapshotPathsWithPolicy(ctx context.Context, repository, directory string, policy *SnapshotPolicy) (string, []string, error) {
	path := filepath.Join(directory, "capture-paths")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = file.Close() }()
	var selected bytes.Buffer
	writer := &snapshotPathBudgetWriter{destination: &selected, remaining: maxSnapshotPathBytes}
	if err := recoveryGit(ctx, repository, writer, "ls-files", "--others", "--exclude-standard", "-z"); err != nil {
		return "", nil, fmt.Errorf("select recovery snapshot paths: %w", err)
	}
	filtered, err := filterSnapshotPaths(selected.Bytes(), policy)
	if err != nil {
		return "", nil, err
	}
	excluded, err := writeExistingSnapshotPaths(repository, filtered, file)
	if err != nil {
		return "", nil, err
	}
	if err := file.Close(); err != nil {
		return "", nil, err
	}
	return path, excluded, nil
}

func writeExistingSnapshotPaths(repository string, data []byte, destination io.Writer) ([]string, error) {
	var excluded []string
	exclusionBytes := 0
	for len(data) > 0 {
		name, rest, complete := bytes.Cut(data, []byte{0})
		if !complete {
			return nil, fmt.Errorf("invalid recovery snapshot path list after %q", name)
		}
		path := string(name)
		if !filepath.IsLocal(path) {
			if runtime.GOOS == "windows" && lexicallyLocalSnapshotPath(path) {
				// Git can enumerate literal DOS device names created by a
				// POSIX shell, but native Windows APIs cannot safely capture them.
				exclusionBytes += len(snapshotExclusionAnnotation(path))
				if exclusionBytes > maxSnapshotExclusionBytes {
					return nil, fmt.Errorf("recovery snapshot cannot classify excluded Windows reserved path %q: exclusion byte budget exceeded", path)
				}
				excluded = append(excluded, path)
				data = rest
				continue
			}
			return nil, fmt.Errorf("invalid recovery snapshot path %q", path)
		}
		data = rest
		info, err := os.Lstat(filepath.Join(repository, path))
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("recovery snapshot path disappeared after selection: %q", name)
		} else if err != nil {
			return nil, err
		}
		if info.IsDir() {
			if _, err := os.Lstat(filepath.Join(repository, path, ".git")); err == nil {
				return nil, ErrNestedRecoveryRequired
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		if _, err := destination.Write(append(name[:len(name):len(name)], 0)); err != nil {
			return nil, err
		}
	}
	return excluded, nil
}

func snapshotExclusionAnnotation(path string) string {
	return fmt.Sprintf("\n\nRecovery-Excluded-Path: %q\nRecovery-Exclusion-Reason: windows-reserved-device-name", path)
}

func lexicallyLocalSnapshotPath(path string) bool {
	if path == "" || filepath.VolumeName(path) != "" || strings.Contains(path, ":") ||
		strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) {
		return false
	}
	clean := filepath.Clean(path)
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
