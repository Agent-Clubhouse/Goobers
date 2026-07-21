package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// status records why a worktree's marker is on disk, distinguishing an
// in-flight run from one that was intentionally kept after failure for
// debugging (KeepOnFailure). Reap treats the two differently: active markers
// with a dead owning process are always crash orphans; kept markers are only
// swept up once they age past ReapOptions.StaleAfter.
type status string

const (
	statusActive status = "active"
	statusKept   status = "kept"
)

// marker is the on-disk record placed alongside each worktree. It carries
// enough state for Manager.Reap to tell a live run apart from one whose
// owning process died mid-stage.
type marker struct {
	RunID          string    `json:"run_id"`
	OwnerRunID     string    `json:"owner_run_id,omitempty"`
	Branch         string    `json:"branch,omitempty"`
	StartRef       string    `json:"start_ref,omitempty"`
	AssetPathGuard bool      `json:"asset_path_guard,omitempty"`
	PID            int       `json:"pid"`
	CreatedAt      time.Time `json:"created_at"`
	RetainedAt     time.Time `json:"retained_at,omitempty"`
	Status         status    `json:"status"`
	SizeBytes      *int64    `json:"size_bytes,omitempty"`
}

func (m marker) retainedAt() time.Time {
	if !m.RetainedAt.IsZero() {
		return m.RetainedAt
	}
	return m.CreatedAt
}

func writeMarker(path string, m marker) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("worktree: create marker dir: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("worktree: encode marker: %w", err)
	}
	// Write to a temp file, fsync it, rename, then fsync the parent
	// directory — a rename alone can still leave a torn or entirely absent
	// marker after a crash on filesystems that don't guarantee rename
	// durability without an explicit directory fsync (issue #136).
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("worktree: write marker: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("worktree: write marker: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("worktree: fsync marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("worktree: close marker: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("worktree: commit marker: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("worktree: fsync marker dir: %w", err)
	}
	return nil
}

// fsyncDir fsyncs a directory so a preceding rename into it is durable
// across a crash — mirrors internal/journal/fsio.go's fsyncDir; duplicated
// rather than shared since internal/worktree has no other reason to depend
// on internal/journal. Directory fsync is unsupported on some
// platforms/filesystems; EINVAL/ENOTSUP from that case is tolerated (the
// write itself already landed, just without the extra durability guarantee)
// rather than turning every worktree.Create into a hard failure on those
// systems — anything else is a real error and is surfaced.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return err
	}
	return nil
}

func readMarker(path string) (marker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return marker{}, err
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil {
		return marker{}, fmt.Errorf("worktree: decode marker %s: %w", path, err)
	}
	return m, nil
}
