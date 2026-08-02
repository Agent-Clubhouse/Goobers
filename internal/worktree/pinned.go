package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
)

// PinnedCleanPolicy controls untracked-file cleanup before a pinned run.
type PinnedCleanPolicy string

const (
	PinnedCleanNone        PinnedCleanPolicy = "none"
	PinnedCleanIgnoredSafe PinnedCleanPolicy = "ignored-safe"
	PinnedCleanFull        PinnedCleanPolicy = "full"
)

// PinnedOptions configures one whole-run pinned-workspace lease.
type PinnedOptions struct {
	RepoURL               string
	RunID                 string
	BaseRef               string
	Branch                string
	RequireExistingBranch bool
	SyncBase              bool
	CleanPolicy           PinnedCleanPolicy
	OnQueuePosition       func(int) error
}

type pinnedLeaseRecord struct {
	RunID     string    `json:"run_id"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// StalePinnedLeaseError requires the operator reset path to recover a workspace
// whose prior process died while holding it.
type StalePinnedLeaseError struct {
	RunID string
}

func (e *StalePinnedLeaseError) Error() string {
	return fmt.Sprintf("worktree: pinned workspace has stale lease from run %q; explicit workspace reset is required", e.RunID)
}

// PinnedLease owns a pinned workspace for an entire run.
type PinnedLease struct {
	Worktree *Worktree
	handle   *lock.Handle
	queue    string
	record   string
}

// Release relinquishes the whole-run lease without removing the workspace.
func (l *PinnedLease) Release() error {
	if l == nil {
		return nil
	}
	var err error
	if l.record != "" {
		err = os.WriteFile(l.record, nil, 0o644)
	}
	err = errors.Join(err, l.handle.Release())
	if l.queue != "" {
		if removeErr := os.Remove(l.queue); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}

// AcquirePinned waits in FIFO order for the repository's whole-run lease,
// prepares its stable workspace, and returns it without creating a git worktree.
func (m *Manager) AcquirePinned(ctx context.Context, opts PinnedOptions) (_ *PinnedLease, retErr error) {
	if !validRunID(opts.RunID) {
		return nil, fmt.Errorf("worktree: pinned RunID %q must be a single path segment", opts.RunID)
	}
	if opts.RepoURL == "" || opts.BaseRef == "" {
		return nil, fmt.Errorf("worktree: pinned RepoURL and BaseRef are required")
	}
	switch opts.CleanPolicy {
	case "", PinnedCleanNone, PinnedCleanIgnoredSafe, PinnedCleanFull:
	default:
		return nil, fmt.Errorf("worktree: unknown pinned clean policy %q", opts.CleanPolicy)
	}

	key := repoKey(opts.RepoURL)
	root := filepath.Join(m.pinnedRoot, key)
	queueDir := filepath.Join(root, "lease.queue")
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		return nil, fmt.Errorf("worktree: create pinned lease queue: %w", err)
	}
	queueFile, err := os.CreateTemp(queueDir, fmt.Sprintf("%020d-%d-%s-", time.Now().UnixNano(), os.Getpid(), opts.RunID))
	if err != nil {
		return nil, fmt.Errorf("worktree: enqueue pinned run: %w", err)
	}
	queuePath := queueFile.Name()
	if closeErr := queueFile.Close(); closeErr != nil {
		_ = os.Remove(queuePath)
		return nil, fmt.Errorf("worktree: close pinned queue entry: %w", closeErr)
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(queuePath)
		}
	}()

	lockPath := filepath.Join(root, "pin.lock")
	recordPath := filepath.Join(root, "pin.lease.json")
	var handle *lock.Handle
	lastPosition := -1
	for {
		entries, err := os.ReadDir(queueDir)
		if err != nil {
			return nil, fmt.Errorf("worktree: read pinned lease queue: %w", err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		position := 0
		for i, name := range names {
			if filepath.Join(queueDir, name) == queuePath {
				position = i + 1
				break
			}
		}
		if position == 0 {
			return nil, fmt.Errorf("worktree: pinned queue entry disappeared for run %s", opts.RunID)
		}
		visiblePosition := position
		if data, readErr := os.ReadFile(recordPath); readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
			visiblePosition++
		} else if readErr != nil && !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("worktree: read pinned lease position: %w", readErr)
		}
		if visiblePosition != lastPosition && opts.OnQueuePosition != nil {
			if err := opts.OnQueuePosition(visiblePosition); err != nil {
				return nil, err
			}
			lastPosition = visiblePosition
		}
		if position == 1 {
			handle, err = lock.TryAcquire(lockPath)
			if err == nil {
				break
			}
			if !errors.Is(err, lock.ErrHeld) {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	lease := &PinnedLease{handle: handle, queue: queuePath, record: recordPath}
	defer func() {
		if retErr != nil {
			_ = lease.Release()
		}
	}()
	if err := os.Remove(queuePath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("worktree: dequeue acquired pinned run: %w", err)
	}
	lease.queue = ""
	if data, err := os.ReadFile(recordPath); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		var stale pinnedLeaseRecord
		if decodeErr := json.Unmarshal(data, &stale); decodeErr != nil {
			return nil, fmt.Errorf("worktree: decode pinned lease record: %w", decodeErr)
		}
		lease.record = ""
		return nil, &StalePinnedLeaseError{RunID: stale.RunID}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("worktree: read pinned lease record: %w", err)
	}
	record, _ := json.Marshal(pinnedLeaseRecord{RunID: opts.RunID, PID: os.Getpid(), StartedAt: time.Now().UTC()})
	if err := os.WriteFile(recordPath, record, 0o644); err != nil {
		return nil, fmt.Errorf("worktree: persist pinned lease: %w", err)
	}

	wt, err := m.preparePinned(ctx, key, opts)
	if err != nil {
		return nil, err
	}
	lease.Worktree = wt
	return lease, nil
}

func (m *Manager) preparePinned(ctx context.Context, key string, opts PinnedOptions) (*Worktree, error) {
	root := filepath.Join(m.pinnedRoot, key)
	repoDir := filepath.Join(root, "repo.git")
	pinDir := filepath.Join(root, "pin")
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		if err := os.MkdirAll(repoDir, 0o755); err != nil {
			return nil, fmt.Errorf("worktree: create pinned mirror: %w", err)
		}
		if err := runGit(ctx, repoDir, "init", "--bare"); err != nil {
			return nil, fmt.Errorf("worktree: initialize pinned mirror: %w", err)
		}
		if err := runGit(ctx, repoDir, "remote", "add", "origin", opts.RepoURL); err != nil {
			return nil, fmt.Errorf("worktree: configure pinned mirror origin: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("worktree: stat pinned mirror: %w", err)
	}
	refspecs := []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}
	if err := m.runRemoteGit(ctx, opts.RepoURL, repoDir, append([]string{"fetch", "--prune", "origin"}, refspecs...)...); err != nil {
		return nil, fmt.Errorf("worktree: fetch pinned mirror: %w", err)
	}
	if err := ensureManagedGitConfig(ctx, repoDir); err != nil {
		return nil, err
	}
	createdPin := false
	if _, err := os.Stat(pinDir); os.IsNotExist(err) {
		if err := runGit(ctx, root, "clone", "--no-checkout", repoDir, pinDir); err != nil {
			return nil, fmt.Errorf("worktree: materialize pinned workspace: %w", err)
		}
		if err := runGit(ctx, pinDir, "remote", "rename", "origin", "mirror"); err != nil {
			return nil, fmt.Errorf("worktree: name pinned mirror remote: %w", err)
		}
		if err := runGit(ctx, pinDir, "remote", "add", "origin", opts.RepoURL); err != nil {
			return nil, fmt.Errorf("worktree: configure pinned push remote: %w", err)
		}
		createdPin = true
	} else if err != nil {
		return nil, fmt.Errorf("worktree: stat pinned workspace: %w", err)
	}
	if !createdPin {
		if err := runGit(ctx, pinDir, "fetch", "--prune", "mirror", "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
			return nil, fmt.Errorf("worktree: refresh pinned workspace refs: %w", err)
		}
	}
	existing := opts.Branch != "" && branchExists(ctx, pinDir, opts.Branch)
	switch {
	case opts.Branch == "":
		if err := runGit(ctx, pinDir, "checkout", "--detach", "--force", opts.BaseRef); err != nil {
			return nil, fmt.Errorf("worktree: checkout pinned base: %w", err)
		}
	case existing:
		if err := runGit(ctx, pinDir, "checkout", "--force", opts.Branch); err != nil {
			return nil, fmt.Errorf("worktree: checkout pinned branch: %w", err)
		}
	case opts.RequireExistingBranch:
		return nil, fmt.Errorf("worktree: branch %q does not exist in pinned workspace for run %s", opts.Branch, opts.RunID)
	default:
		if err := runGit(ctx, pinDir, "checkout", "-b", opts.Branch, opts.BaseRef); err != nil {
			return nil, fmt.Errorf("worktree: create pinned branch: %w", err)
		}
	}
	if err := runGit(ctx, pinDir, "reset", "--hard", "HEAD"); err != nil {
		return nil, fmt.Errorf("worktree: reset pinned workspace: %w", err)
	}
	switch opts.CleanPolicy {
	case PinnedCleanIgnoredSafe:
		if err := runGit(ctx, pinDir, "clean", "-ffd"); err != nil {
			return nil, fmt.Errorf("worktree: clean pinned workspace: %w", err)
		}
	case PinnedCleanFull:
		if err := runGit(ctx, pinDir, "clean", "-ffdx"); err != nil {
			return nil, fmt.Errorf("worktree: fully clean pinned workspace: %w", err)
		}
	}
	if err := runGit(ctx, pinDir, "config", "user.name", botGitUserName); err != nil {
		return nil, err
	}
	if err := runGit(ctx, pinDir, "config", "user.email", botGitUserEmail); err != nil {
		return nil, err
	}
	if err := ensureScratchExcluded(ctx, pinDir); err != nil {
		return nil, err
	}
	startRef, err := gitOutput(ctx, pinDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if opts.SyncBase && existing {
		if err := runGit(ctx, pinDir, "merge", "--ff", "--no-edit", opts.BaseRef); err != nil {
			return nil, fmt.Errorf("worktree: sync pinned branch %q with base %q: %w", opts.Branch, opts.BaseRef, err)
		}
	}
	return &Worktree{
		RunID: opts.RunID, Path: pinDir, Branch: opts.Branch,
		manager: m, key: key, startRef: startRef, repoURL: opts.RepoURL,
		pinned: true, repoDir: pinDir,
	}, nil
}
