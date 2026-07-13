package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ReapOptions configures orphan detection for Manager.Reap.
type ReapOptions struct {
	// StaleAfter additionally reaps kept worktrees (RemoveOptions.Keep) once
	// they age past this duration. Zero leaves kept worktrees alone
	// indefinitely — Reap then only clears genuine crash orphans.
	StaleAfter time.Duration
}

// ReapReason explains why Reap removed a worktree.
type ReapReason string

const (
	// ReapReasonOrphaned means the worktree's owning process is no longer
	// alive but never called Remove — a crash mid-run (e.g. kill -9).
	ReapReasonOrphaned ReapReason = "orphaned"
	// ReapReasonStale means the worktree was intentionally kept
	// (RemoveOptions.Keep) and has aged past ReapOptions.StaleAfter.
	ReapReasonStale ReapReason = "stale"
)

// ReapResult reports one worktree that Reap removed.
type ReapResult struct {
	RunID  string
	Path   string
	Reason ReapReason
}

// Reap scans every managed working copy under Root for worktrees whose
// marker shows either a dead owning process (a crash orphan) or a
// keep-on-failure worktree older than opts.StaleAfter, and removes them.
// Call it on daemon start so a restart converges disk state after a crash
// without operator intervention.
//
// A live run in progress is never touched: its marker's PID is the current
// process (Manager.Create stamps os.Getpid()), which is always alive from
// its own perspective.
func (m *Manager) Reap(ctx context.Context, opts ReapOptions) ([]ReapResult, error) {
	repoDirs, err := os.ReadDir(m.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worktree: list root %s: %w", m.Root, err)
	}

	var results []ReapResult
	for _, rd := range repoDirs {
		if !rd.IsDir() {
			continue
		}
		key := rd.Name()
		found, err := m.reapRepo(ctx, key, opts)
		if err != nil {
			return results, err
		}
		results = append(results, found...)
	}
	return results, nil
}

func (m *Manager) reapRepo(ctx context.Context, key string, opts ReapOptions) ([]ReapResult, error) {
	markersDir := m.markersDirForKey(key)
	entries, err := os.ReadDir(markersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worktree: list markers for %s: %w", key, err)
	}

	var results []ReapResult
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		markerPath := filepath.Join(markersDir, e.Name())
		mk, err := readMarker(markerPath)
		if err != nil {
			return results, fmt.Errorf("worktree: read marker %s: %w", markerPath, err)
		}

		var reason ReapReason
		switch mk.Status {
		case statusActive:
			if processAlive(mk.PID) {
				continue
			}
			reason = ReapReasonOrphaned
		case statusKept:
			if opts.StaleAfter <= 0 || time.Since(mk.CreatedAt) <= opts.StaleAfter {
				continue
			}
			reason = ReapReasonStale
		default:
			continue
		}

		path := filepath.Join(m.runsDirForKey(key), mk.RunID)
		if err := m.reapOne(ctx, key, path, markerPath); err != nil {
			return results, fmt.Errorf("worktree: reap run %s: %w", mk.RunID, err)
		}
		results = append(results, ReapResult{RunID: mk.RunID, Path: path, Reason: reason})
	}
	return results, nil
}

func (m *Manager) reapOne(ctx context.Context, key, path, markerPath string) error {
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	repoDir := m.repoDirForKey(key)
	if err := runGit(ctx, repoDir, "worktree", "remove", "--force", path); err != nil {
		// The worktree directory itself may already be gone (e.g. the crash
		// happened mid-remove); prune the administrative metadata instead of
		// failing the whole reap pass.
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			if pruneErr := runGit(ctx, repoDir, "worktree", "prune"); pruneErr != nil {
				return pruneErr
			}
		} else {
			return err
		}
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("worktree: remove marker %s: %w", markerPath, err)
	}
	return nil
}

// processAlive reports whether pid names a live process, using signal 0 to
// probe without actually signaling it. This is a best-effort Unix check: PID
// reuse after a reboot can in principle produce a false "alive" for an
// unrelated process, which is an accepted limitation at V0.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
