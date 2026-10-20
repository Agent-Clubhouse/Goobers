package interactiveexec

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/invoke"
)

// CaptureRunSnapshot copies an initialized, idle parent into a private mutable
// run workspace. The host must hold its session coordinator's idle-snapshot
// guard for this entire call; this method never calls the coordinator, launches
// commands, or modifies the parent. A key is reserved once, including failed or
// uncertain copies, and must never be used to adopt an existing directory.
func (e *Executor) CaptureRunSnapshot(ctx context.Context, owner, sessionID, runKey string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if owner != e.config.Principal.Identity().Subject {
		return "", fmt.Errorf("snapshot owner mismatch")
	}
	if !safeID.MatchString(runKey) {
		return "", fmt.Errorf("invalid server run identity")
	}
	parent, err := e.workspace(sessionID)
	if err != nil {
		return "", err
	}
	if err := e.snapshotParentReady(parent); err != nil {
		return "", err
	}
	base := filepath.Join(e.config.StateDir, "native-snapshots")
	if err := ensureSnapshotDirectory(base); err != nil {
		return "", err
	}
	sessionDir := filepath.Join(base, sessionID)
	if err := ensureSnapshotDirectory(sessionDir); err != nil {
		return "", err
	}
	runDir := filepath.Join(sessionDir, fmt.Sprintf("%x", sha256.Sum256([]byte(runKey))))
	// Mkdir is the exclusive reservation. It refuses both a completed snapshot
	// and an incomplete/crashed attempt; rename must never overwrite either.
	if err := os.Mkdir(runDir, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("run snapshot key already reserved; existing or uncertain snapshot cannot be reused")
		}
		return "", err
	}
	temporary, err := os.MkdirTemp(runDir, ".source-*")
	if err != nil {
		return "", err
	}
	// Best-effort cleanup; the operation result is reported separately.
	defer func() { _ = os.RemoveAll(temporary) }()
	if err := copySourceContext(ctx, parent, temporary); err != nil {
		return "", err
	}
	if err := e.snapshotParentReady(parent); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target := filepath.Join(runDir, "source")
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("run snapshot target unexpectedly exists or is unavailable")
	}
	if err := os.Rename(temporary, target); err != nil {
		return "", err
	}
	return target, nil
}

func ensureSnapshotDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("run snapshots require private ordinary directories")
	}
	return nil
}

func (e *Executor) snapshotParentReady(parent string) error {
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("parent snapshot must be an ordinary directory")
	}
	if _, err := os.Lstat(e.metadata(parent, "writer-active")); err == nil {
		return fmt.Errorf("parent writer custody is unconfirmed: %w", invoke.ErrWorkspaceNotQuiescent)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	baseline, err := os.Lstat(e.metadata(parent, "baseline"))
	if err != nil || !baseline.Mode().IsRegular() || baseline.Size() != 40 || !e.hasBaseline(parent) {
		return fmt.Errorf("parent snapshot baseline is unavailable or incomplete")
	}
	return nil
}
