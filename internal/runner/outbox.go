package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
)

const (
	outboxExportFailureCode       = "outbox_export_failed"
	outboxExportFailureAnnotation = "outbox.export.failed"
	maxOutboxDiagnosticFiles      = 3
)

type outboxFileDiagnostic struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type outboxCandidate struct {
	relPath string
	absPath string
	size    int64
}

type outboxLimitError struct {
	FileCount      int
	FileLimit      int
	AggregateBytes int64
	ByteLimit      int64
	LargestFiles   []outboxFileDiagnostic
}

func (e *outboxLimitError) Error() string {
	parts := make([]string, 0, len(e.LargestFiles))
	for _, file := range e.LargestFiles {
		parts = append(parts, fmt.Sprintf("%s (%d bytes)", file.Path, file.Size))
	}
	detail := ""
	if len(parts) > 0 {
		detail = "; largest observed files: " + strings.Join(parts, ", ")
	}
	if e.FileCount > e.FileLimit {
		return fmt.Sprintf("outbox export observed %d files, exceeds the %d-file limit (observed aggregate %d bytes)%s",
			e.FileCount, e.FileLimit, e.AggregateBytes, detail)
	}
	return fmt.Sprintf("outbox export totals %d bytes across %d files, exceeds the %d-byte aggregate limit%s",
		e.AggregateBytes, e.FileCount, e.ByteLimit, detail)
}

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

// finalizeOutbox keeps post-command collection failures on the result path:
// the command's own outcome is journaled separately, while the stage result is
// failed because its declared evidence was not durably delivered. Only a
// failure to write that diagnostic annotation remains a dispatch error.
func (r *Runner) finalizeOutbox(jr executionJournal, workspaceRoot string, t apiv1.Task, attempt int, class journal.AttemptClass, commandResult apiv1.ResultEnvelope) (apiv1.ResultEnvelope, error) {
	exportErr := r.exportOutbox(jr, workspaceRoot, t, attempt, class)
	if exportErr == nil {
		return commandResult, nil
	}
	if err := recordOutboxExportFailure(jr, t, attempt, class, commandResult, exportErr); err != nil {
		return commandResult, fmt.Errorf("record outbox export failure: %w", errors.Join(exportErr, err))
	}
	return outboxExportFailureResult(commandResult, exportErr), nil
}

func recordOutboxExportFailure(jr executionJournal, t apiv1.Task, attempt int, class journal.AttemptClass, commandResult apiv1.ResultEnvelope, exportErr error) error {
	fields := map[string]any{
		"kind":                     outboxExportFailureAnnotation,
		"commandStatus":            string(commandResult.Status),
		"commandSummary":           commandResult.Summary,
		"commandArtifactCount":     len(commandResult.Artifacts),
		"commandOutputCount":       len(commandResult.Outputs),
		"outboxEvidenceState":      "missing-or-partial",
		"outboxExportErrorCode":    outboxExportFailureCode,
		"outboxExportErrorMessage": exportErr.Error(),
	}
	if commandResult.Error != nil {
		fields["commandErrorCode"] = commandResult.Error.Code
		fields["commandErrorMessage"] = commandResult.Error.Message
		fields["commandErrorRetryable"] = commandResult.Error.Retryable
	}
	if exitCode, ok := commandResult.Metrics["exitCode"]; ok {
		fields["commandExitCode"] = exitCode
	}
	var limitErr *outboxLimitError
	if errors.As(exportErr, &limitErr) {
		fields["outboxEvidenceState"] = "missing"
		fields["outboxFileCount"] = limitErr.FileCount
		fields["outboxFileLimit"] = limitErr.FileLimit
		fields["outboxAggregateBytes"] = limitErr.AggregateBytes
		fields["outboxAggregateByteLimit"] = limitErr.ByteLimit
		fields["outboxLargestFiles"] = limitErr.LargestFiles
	}
	return jr.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, Stage: t.Name, Attempt: attempt, AttemptClass: class,
		Runner: fields,
	})
}

func outboxExportFailureResult(commandResult apiv1.ResultEnvelope, exportErr error) apiv1.ResultEnvelope {
	status := commandResult.Status
	commandResult.Status = apiv1.ResultFailure
	commandResult.Summary = fmt.Sprintf("command reported %s; required outbox export failed", status)
	commandResult.Error = &apiv1.ErrorInfo{
		Code:    outboxExportFailureCode,
		Message: exportErr.Error(),
	}
	return commandResult
}

func isOutboxExportFailure(result apiv1.ResultEnvelope) bool {
	return result.Error != nil && result.Error.Code == outboxExportFailureCode
}

// taskDispatchError normalizes an outbox collection failure onto the same
// branch-failure path as a dispatch error without misreporting it as an
// executor_error. The command and export outcomes were already journaled by
// runTask before this boundary.
func taskDispatchError(stage string, result apiv1.ResultEnvelope, dispatchErr error) error {
	if dispatchErr != nil || !isOutboxExportFailure(result) {
		return dispatchErr
	}
	return codedStageFailure(outboxExportFailureCode, fmt.Errorf("stage %q: %s", stage, result.Error.Message))
}

func stageFinishedOutputs(result apiv1.ResultEnvelope, continueOnError bool) map[string]interface{} {
	if result.Status == apiv1.ResultFailure && continueOnError && !isOutboxExportFailure(result) {
		return nil
	}
	return result.Outputs
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
	var candidates []outboxCandidate
	var totalBytes int64

	addFile := func(relPath, absPath string, size int64) error {
		candidates = append(candidates, outboxCandidate{relPath: relPath, absPath: absPath, size: size})
		totalBytes += size
		if len(candidates) > journal.MaxOutboxFilesPerAttempt {
			return newOutboxLimitError(candidates, totalBytes)
		}
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
			opts := mcpio.DefaultWalkFilesOptions()
			// Hidden directories are part of the declared workspace content and
			// must not be silently pruned from exported outbox payloads.
			opts.SkipHiddenDirs = false
			walkErr := mcpio.WalkFiles(full, func(p string, d fs.DirEntry) error {
				rel, err := filepath.Rel(resolvedRoot, p)
				if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
					return fmt.Errorf("resolve outbox file %q relative to workspace: %w", p, err)
				}
				fi, err := d.Info()
				if err != nil {
					return fmt.Errorf("stat outbox file %q: %w", p, err)
				}
				return addFile(filepath.ToSlash(rel), p, fi.Size())
			}, opts)
			if walkErr != nil {
				return nil, fmt.Errorf("declared outbox path %q: %w", entry, walkErr)
			}
		default:
			// Not a regular file or directory (device, socket, ...) — skip.
		}
	}
	if totalBytes > journal.MaxOutboxBytesPerAttempt {
		return nil, newOutboxLimitError(candidates, totalBytes)
	}

	files := make([]journal.OutboxFile, 0, len(candidates))
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate.absPath)
		if err != nil {
			return nil, fmt.Errorf("read outbox file %q: %w", candidate.relPath, err)
		}
		files = append(files, journal.OutboxFile{RelPath: candidate.relPath, Data: data})
	}
	return files, nil
}

func newOutboxLimitError(candidates []outboxCandidate, totalBytes int64) *outboxLimitError {
	largest := append([]outboxCandidate(nil), candidates...)
	sort.Slice(largest, func(i, j int) bool {
		if largest[i].size != largest[j].size {
			return largest[i].size > largest[j].size
		}
		return largest[i].relPath < largest[j].relPath
	})
	if len(largest) > maxOutboxDiagnosticFiles {
		largest = largest[:maxOutboxDiagnosticFiles]
	}
	diagnostics := make([]outboxFileDiagnostic, 0, len(largest))
	for _, file := range largest {
		diagnostics = append(diagnostics, outboxFileDiagnostic{Path: file.relPath, Size: file.size})
	}
	return &outboxLimitError{
		FileCount:      len(candidates),
		FileLimit:      journal.MaxOutboxFilesPerAttempt,
		AggregateBytes: totalBytes,
		ByteLimit:      journal.MaxOutboxBytesPerAttempt,
		LargestFiles:   diagnostics,
	}
}
