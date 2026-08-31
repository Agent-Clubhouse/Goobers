package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// exportOutbox durably exports a stage's declared apiv1.Task.Outbox paths
// (#1552) from workspaceRoot into the run journal, after the stage's
// executor has returned successfully and before dispatchTask's deferred
// workspace teardown runs. It is a pure no-op when the task declares no
// outbox paths, so stages that never opt in are unaffected.
//
// Every declared path is resolved with apiv1.ResolveContainedPath — the
// same containment primitive the harness's liftArtifactFile uses for its
// single declared artifact file — so a path that escapes workspaceRoot,
// lexically or via a symlink, fails the stage closed rather than lifting an
// arbitrary host file into the journal. A declared path that simply does
// not exist is skipped, not an error: outbox output is supplementary to a
// stage's ordinary result, not a required deliverable.
func (r *Runner) exportOutbox(jr executionJournal, workspaceRoot string, t apiv1.Task, attempt int, class journal.AttemptClass) error {
	if len(t.Outbox) == 0 {
		return nil
	}
	files, err := collectOutboxFiles(workspaceRoot, t.Outbox)
	if err != nil {
		return fmt.Errorf("task %q: collect outbox files: %w", t.Name, err)
	}
	if len(files) == 0 {
		return nil
	}
	refs, err := jr.ExportOutbox(t.Name, attempt, class, files)
	if err != nil {
		return fmt.Errorf("task %q: export outbox: %w", t.Name, err)
	}
	if t.OutboxMirrorPath != "" {
		if err := mirrorOutbox(jr.Dir(), t.OutboxMirrorPath, refs); err != nil {
			return fmt.Errorf("task %q: mirror outbox: %w", t.Name, err)
		}
	}
	return nil
}

func mirrorOutbox(runDir, configuredRoot string, refs []journal.Ref) error {
	root, err := expandOutboxMirrorRoot(configuredRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create mirror root: %w", err)
	}
	runID := filepath.Base(runDir)
	if !apiv1.ValidRunID(runID) {
		return fmt.Errorf("invalid run id %q", runID)
	}
	const outboxPrefix = "artifacts/outbox/"
	for _, ref := range refs {
		refPath := filepath.ToSlash(ref.Path)
		if !strings.HasPrefix(refPath, outboxPrefix) {
			return fmt.Errorf("journal ref %q is outside the outbox", ref.Path)
		}
		source, err := apiv1.ResolveContainedPath(runDir, filepath.FromSlash(refPath))
		if err != nil {
			return fmt.Errorf("resolve journal source %q: %w", ref.Path, err)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("read journal source %q: %w", ref.Path, err)
		}
		rel := filepath.Join(runID, filepath.FromSlash(strings.TrimPrefix(refPath, outboxPrefix)))
		parent, err := makeContainedDir(root, filepath.Dir(rel))
		if err != nil {
			return fmt.Errorf("prepare mirror destination %q: %w", rel, err)
		}
		tmp, err := os.CreateTemp(parent, ".outbox-*")
		if err != nil {
			return fmt.Errorf("create mirror temporary file: %w", err)
		}
		tmpName := tmp.Name()
		err = tmp.Chmod(0o644)
		if err == nil {
			_, err = tmp.Write(data)
		}
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			dest := filepath.Join(parent, filepath.Base(rel))
			if info, statErr := os.Lstat(dest); statErr == nil {
				if info.IsDir() {
					err = fmt.Errorf("destination is a directory")
				}
				if _, resolveErr := apiv1.ResolveContainedPath(root, rel); resolveErr != nil {
					err = resolveErr
				} else if err == nil {
					err = os.Remove(dest)
				}
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				err = statErr
			}
			if err == nil {
				err = os.Rename(tmpName, dest)
			}
		}
		if err != nil {
			_ = os.Remove(tmpName)
			return fmt.Errorf("write mirror destination %q: %w", rel, err)
		}
	}
	return nil
}

func expandOutboxMirrorRoot(configured string) (string, error) {
	if err := apiv1.ValidateOutboxMirrorRoot(configured); err != nil {
		return "", err
	}
	root := configured
	if strings.HasPrefix(root, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		root = filepath.Join(home, strings.TrimPrefix(root, "~/"))
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("mirror root %q must be absolute or start with ~/", configured)
	}
	return filepath.Clean(root), nil
}

// makeContainedDir creates a relative directory one segment at a time and
// checks every resulting path after symlink resolution.
func makeContainedDir(root, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", apiv1.ErrPathEscape
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	current := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return "", err
		}
		back, err := filepath.Rel(resolvedRoot, resolved)
		if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%w: %q resolves to %q", apiv1.ErrSymlinkEscape, rel, resolved)
		}
	}
	return filepath.EvalSymlinks(current)
}

// collectOutboxFiles resolves each declared entry against workspaceRoot and
// reads the matching regular file(s) into memory, ready for
// journal.Run.ExportOutbox. It enforces the same aggregate file-count and
// byte-size ceiling ExportOutbox itself enforces — not to avoid that check
// (ExportOutbox re-validates everything unconditionally; nothing here is
// trusted as a substitute), but so a runaway declared directory is rejected
// before its bytes are all read into memory, not after.
//
// A directory entry is walked with filepath.WalkDir, which never follows
// symlinks: a symlinked subdirectory is reported as a symlink dirent
// (IsDir() false) and is not recursed into, and a symlinked file dirent is
// skipped outright rather than dereferenced. Combined with
// ResolveContainedPath's symlink-aware containment check on every declared
// top-level entry, no file this function returns can have reached the batch
// via an unvalidated symlink hop.
func collectOutboxFiles(workspaceRoot string, declared []string) ([]journal.OutboxFile, error) {
	var files []journal.OutboxFile
	var totalBytes int64

	addFile := func(relPath, absPath string, size int64) error {
		if len(files)+1 > journal.MaxOutboxFilesPerAttempt {
			return fmt.Errorf("outbox declares more than %d files", journal.MaxOutboxFilesPerAttempt)
		}
		totalBytes += size
		if totalBytes > journal.MaxOutboxBytesPerAttempt {
			return fmt.Errorf("outbox export exceeds the %d-byte aggregate limit", journal.MaxOutboxBytesPerAttempt)
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return fmt.Errorf("read outbox file %q: %w", relPath, err)
		}
		files = append(files, journal.OutboxFile{RelPath: relPath, Data: data})
		return nil
	}

	resolvedRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}

	for _, entry := range declared {
		full, err := apiv1.ResolveContainedPath(workspaceRoot, entry)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("declared outbox path %q: %w", entry, err)
		}
		info, err := os.Stat(full)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat declared outbox path %q: %w", entry, err)
		}
		switch {
		case info.Mode().IsRegular():
			if err := addFile(entry, full, info.Size()); err != nil {
				return nil, fmt.Errorf("declared outbox path %q: %w", entry, err)
			}
		case info.IsDir():
			walkErr := filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.Type()&fs.ModeSymlink != 0 {
					return nil
				}
				if d.IsDir() {
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				rel, err := filepath.Rel(resolvedRoot, p)
				if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
					return fmt.Errorf("resolve outbox file %q relative to workspace: %w", p, err)
				}
				fi, err := d.Info()
				if err != nil {
					return fmt.Errorf("stat outbox file %q: %w", p, err)
				}
				return addFile(filepath.ToSlash(rel), p, fi.Size())
			})
			if walkErr != nil {
				return nil, fmt.Errorf("declared outbox path %q: %w", entry, walkErr)
			}
		default:
			// Not a regular file or directory (device, socket, ...) — skip.
		}
	}
	return files, nil
}
