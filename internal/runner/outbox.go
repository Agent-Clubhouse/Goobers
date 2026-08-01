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
	if _, err := jr.ExportOutbox(t.Name, attempt, class, files); err != nil {
		return fmt.Errorf("task %q: export outbox: %w", t.Name, err)
	}
	return nil
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
