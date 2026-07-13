package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	RunID     string    `json:"run_id"`
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"created_at"`
	Status    status    `json:"status"`
}

func writeMarker(path string, m marker) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("worktree: create marker dir: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("worktree: encode marker: %w", err)
	}
	// Write to a temp file and rename so a crash never leaves a half-written
	// marker that Reap would fail to parse.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("worktree: write marker: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("worktree: commit marker: %w", err)
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
