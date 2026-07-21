package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/proc"
)

// ReapOptions configures orphan detection for Manager.Reap.
type ReapOptions struct {
	// StaleAfter additionally reaps kept worktrees (RemoveOptions.Keep) once
	// they age past this duration. Zero leaves kept worktrees alone
	// indefinitely — Reap then only clears genuine crash orphans.
	StaleAfter time.Duration
	// IsRunTerminal reports whether a markerless, git-deregistered worktree
	// belongs to a terminal run. Nil leaves that ambiguous shape untouched.
	IsRunTerminal func(worktreeID string) (bool, error)
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

// ReapReasonMarkerless means the worktree had no marker at all — a crash
// between `git worktree add` and the marker write (Manager.Create), which
// would otherwise be invisible to Reap forever since the marker-driven scan
// never learns it exists.
const ReapReasonMarkerless ReapReason = "markerless"

// ReapResult reports one worktree that Reap removed.
type ReapResult struct {
	RunID  string
	Path   string
	Reason ReapReason
}

// ReapWarning reports one worktree Reap skipped rather than let abort the
// whole pass.
type ReapWarning struct {
	Path string
	Err  error
}

// Reap scans every managed working copy under Root for worktrees whose
// marker shows either a dead owning process (a crash orphan) or a
// keep-on-failure worktree older than opts.StaleAfter, and removes them. It
// also removes markerless directories still registered with git (a crash
// between `git worktree add` and the marker write) and deregistered
// markerless directories whose owning journal is terminal. Call it on daemon
// start, before resuming any interrupted run, so a restart converges disk
// state after a crash without operator intervention.
//
// A live run in progress is never touched: its marker's PID is the current
// process (Manager.Create stamps os.Getpid()), which is always alive from
// its own perspective.
//
// One unreadable marker is skipped (collected in the returned warnings), not
// fatal to the whole pass — a single corrupt marker must never prevent every
// other repo's genuine orphans from being cleaned up.
func (m *Manager) Reap(ctx context.Context, opts ReapOptions) ([]ReapResult, []ReapWarning, error) {
	defer m.observeUsage(ctx, UsageOperationHousekeeping, "", "", 0, false, nil)
	repoDirs, err := os.ReadDir(m.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("worktree: list root %s: %w", m.Root, err)
	}

	var results []ReapResult
	var warnings []ReapWarning
	for _, rd := range repoDirs {
		if !rd.IsDir() {
			continue
		}
		key := rd.Name()
		found, warned, err := m.reapRepo(ctx, key, opts)
		if err != nil {
			return results, warnings, err
		}
		results = append(results, found...)
		warnings = append(warnings, warned...)
	}
	return results, warnings, nil
}

func (m *Manager) reapRepo(ctx context.Context, key string, opts ReapOptions) ([]ReapResult, []ReapWarning, error) {
	markersDir := m.markersDirForKey(key)
	entries, err := os.ReadDir(markersDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("worktree: list markers for %s: %w", key, err)
	}

	var results []ReapResult
	var warnings []ReapWarning
	seen := map[string]bool{} // RunID (== worktree dir name) with a marker, live or reaped

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		markerPath := filepath.Join(markersDir, e.Name())
		runID := strings.TrimSuffix(e.Name(), ".json")
		mk, err := readMarker(markerPath)
		if err != nil {
			// A single corrupt marker must not abort reaping every other
			// worktree in every other repo (issue #136) — skip it, warn,
			// and keep going. Its worktree is left alone (not markerless,
			// just unreadable) rather than guessed at — mark it seen so the
			// markerless-diff pass below doesn't ALSO sweep it up as if it
			// had no marker at all; an unreadable marker might belong to a
			// genuinely live run, and deleting a live run's worktree would
			// be actively destructive, unlike skipping it.
			warnings = append(warnings, ReapWarning{Path: markerPath, Err: err})
			seen[runID] = true
			continue
		}
		seen[mk.RunID] = true

		var reason ReapReason
		switch mk.Status {
		case statusActive:
			if processAlive(mk.PID) {
				continue
			}
			reason = ReapReasonOrphaned
		case statusKept:
			if opts.StaleAfter <= 0 || time.Since(mk.retainedAt()) <= opts.StaleAfter {
				continue
			}
			reason = ReapReasonStale
		default:
			continue
		}

		path := filepath.Join(m.runsDirForKey(key), mk.RunID)
		if err := m.reapOne(ctx, key, path, markerPath, &mk); err != nil {
			return results, warnings, fmt.Errorf("worktree: reap run %s: %w", mk.RunID, err)
		}
		results = append(results, ReapResult{RunID: mk.RunID, Path: path, Reason: reason})
	}

	markerless, markerlessWarnings, err := m.reapMarkerlessWorktrees(ctx, key, seen, opts)
	if err != nil {
		return results, warnings, err
	}
	results = append(results, markerless...)
	warnings = append(warnings, markerlessWarnings...)
	return results, warnings, nil
}

// reapMarkerlessWorktrees diffs the actual worktree directories under key's
// runs/ against the marker names already accounted for by reapRepo's own
// scan. Registered entries are incomplete creates and are always safe to
// remove; deregistered directories require a terminal owning journal.
func (m *Manager) reapMarkerlessWorktrees(ctx context.Context, key string, seen map[string]bool, opts ReapOptions) ([]ReapResult, []ReapWarning, error) {
	runsDir := m.runsDirForKey(key)
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("worktree: list runs for %s: %w", key, err)
	}

	var results []ReapResult
	var warnings []ReapWarning
	for _, e := range entries {
		if !e.IsDir() || seen[e.Name()] {
			continue
		}
		path := filepath.Join(runsDir, e.Name())
		registered, err := worktreeRegistered(ctx, m.repoDirForKey(key), path)
		if err != nil {
			return results, warnings, fmt.Errorf("worktree: inspect markerless run %s: %w", e.Name(), err)
		}
		if !registered {
			if opts.IsRunTerminal == nil {
				continue
			}
			terminal, err := opts.IsRunTerminal(e.Name())
			if err != nil {
				warnings = append(warnings, ReapWarning{Path: path, Err: err})
				continue
			}
			if !terminal {
				continue
			}
		}
		markerPath := m.markerPath(key, e.Name())
		if err := m.reapOne(ctx, key, path, markerPath, nil); err != nil {
			return results, warnings, fmt.Errorf("worktree: reap markerless run %s: %w", e.Name(), err)
		}
		results = append(results, ReapResult{RunID: e.Name(), Path: path, Reason: ReapReasonMarkerless})
	}
	return results, warnings, nil
}

func (m *Manager) reapOne(ctx context.Context, key, path, markerPath string, mk *marker) error {
	ownerRunID := ""
	worktreeID := filepath.Base(path)
	if mk != nil {
		ownerRunID = mk.OwnerRunID
		worktreeID = mk.RunID
	}
	worktreeBytes, worktreeMeasured, measurementErr := m.measureWorktree(path)
	defer m.observeUsage(
		ctx,
		UsageOperationHousekeeping,
		ownerRunID,
		worktreeID,
		worktreeBytes,
		worktreeMeasured,
		measurementErr,
	)

	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	repoDir := m.repoDirForKey(key)
	if mk != nil {
		if err := m.restoreReservedBranchFromMarker(ctx, key, path, *mk); err != nil {
			return err
		}
	}
	if err := runGit(ctx, repoDir, "worktree", "remove", "--force", path); err != nil {
		// The worktree directory itself may already be gone (e.g. the crash
		// happened mid-remove); prune the administrative metadata instead of
		// failing the whole reap pass.
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			if pruneErr := runGit(ctx, repoDir, "worktree", "prune"); pruneErr != nil {
				return pruneErr
			}
		} else if statErr != nil {
			return fmt.Errorf("worktree: stat %s after remove failed: %w", path, statErr)
		} else {
			registered, inspectErr := worktreeRegistered(ctx, repoDir, path)
			if inspectErr != nil {
				return fmt.Errorf("worktree: inspect registration for %s after remove failed: %w", path, errors.Join(err, inspectErr))
			}
			if registered {
				return err
			}
			if removeErr := os.RemoveAll(path); removeErr != nil {
				return fmt.Errorf("worktree: remove unregistered directory %s: %w", path, removeErr)
			}
		}
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("worktree: remove marker %s: %w", markerPath, err)
	}
	return nil
}

func worktreeRegistered(ctx context.Context, repoDir, path string) (bool, error) {
	out, err := gitOutput(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		registeredPath := strings.TrimPrefix(line, "worktree ")
		if strings.HasPrefix(registeredPath, `"`) {
			registeredPath, err = strconv.Unquote(registeredPath)
			if err != nil {
				return false, fmt.Errorf("worktree: parse registered path %q: %w", registeredPath, err)
			}
		}
		if filepath.Clean(registeredPath) == filepath.Clean(path) {
			return true, nil
		}
		registeredInfo, registeredErr := os.Stat(registeredPath)
		pathInfo, pathErr := os.Stat(path)
		if registeredErr == nil && pathErr == nil && os.SameFile(registeredInfo, pathInfo) {
			return true, nil
		}
	}
	return false, nil
}

// processAlive reports whether pid names a live process. Indirected through
// a var (like newRunID elsewhere) so a test can inject a deterministic check
// instead of depending on a real OS PID belonging to no live process — a
// genuinely-dead PID from a reaped subprocess is inherently racy against PID
// recycling on a busy machine (issue #142, a real QA-gate stress-test flake).
//
// proc.Alive fails toward alive on an ambiguous probe, which is the safe
// direction here: a false "dead" would reap a live run's worktree, while a
// false "alive" only defers a reap.
var processAlive = proc.Alive
