package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/platform/proc"
)

// PinnedCleanPolicy controls untracked-file cleanup before a pinned run.
type PinnedCleanPolicy string

const (
	// PinnedCleanNone preserves ignored and untracked files.
	PinnedCleanNone PinnedCleanPolicy = "none"
	// PinnedCleanIgnoredSafe removes untracked files but preserves ignored files.
	PinnedCleanIgnoredSafe PinnedCleanPolicy = "ignored-safe"
	// PinnedCleanFull removes ignored and untracked files.
	PinnedCleanFull PinnedCleanPolicy = "full"
	// PinnedFailureResetThreshold is the consecutive-failure count at which the
	// runner starts suggesting explicit workspace recovery.
	PinnedFailureResetThreshold = 3
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
	OnQueueWait           func()
}

// PinnedPrepareOptions selects the branch exposed to one serialized stage.
type PinnedPrepareOptions struct {
	BaseRef               string
	Branch                string
	RequireExistingBranch bool
	SyncBase              bool
}

// PinnedResetOptions identifies the workspace to tear down and re-materialize.
type PinnedResetOptions struct {
	RepoURL string
	BaseRef string
}

type pinnedFailureState struct {
	Count int `json:"count"`
}

type pinnedLeaseRecord struct {
	RunID     string    `json:"run_id"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

type pinnedQueueRecord struct {
	RunID        string    `json:"run_id"`
	PID          int       `json:"pid"`
	PIDStartedAt time.Time `json:"pid_started_at,omitempty"`
}

type pinnedQueueEntry struct {
	dir   string
	path  string
	runID string
}

var pinnedQueueProcessAlive = proc.Alive
var pinnedQueueProcessStartTime = proc.StartTime

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
	Worktree    *Worktree
	handle      *lock.Handle
	queue       string
	record      string
	root        string
	releaseOnce sync.Once
	releaseErr  error
}

// Release relinquishes the whole-run lease without removing the workspace.
func (l *PinnedLease) Release() error {
	if l == nil {
		return nil
	}
	l.releaseOnce.Do(func() { l.releaseErr = l.release() })
	return l.releaseErr
}

func (l *PinnedLease) release() error {
	var err error
	if l.Worktree != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err = l.Worktree.manager.handoffPinnedState(ctx, l.Worktree.key, l.Worktree.RunID)
		cancel()
	}
	// A failed final handoff leaves the lease record in place. Another run
	// cannot acquire and reset this state until explicit recovery succeeds.
	if l.record != "" && err == nil {
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
	if err := validatePinnedOptions(opts); err != nil {
		return nil, err
	}

	key := repoKey(opts.RepoURL)
	root := filepath.Join(m.pinnedRoot, key)
	queue, err := enqueuePinnedRun(root, opts.RunID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(queue.path)
		}
	}()

	lockPath := filepath.Join(root, "pin.lock")
	recordPath := filepath.Join(root, "pin.lease.json")
	handle, err := waitForPinnedLease(ctx, queue, lockPath, recordPath, opts)
	if err != nil {
		return nil, err
	}

	lease := &PinnedLease{handle: handle, queue: queue.path, record: recordPath, root: root}
	defer func() {
		if retErr != nil {
			_ = lease.Release()
		}
	}()
	if err := os.Remove(queue.path); err != nil && !os.IsNotExist(err) {
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

	wt, err := m.preparePinnedWithCustody(ctx, key, opts)
	if err != nil {
		return nil, err
	}
	lease.Worktree = wt
	return lease, nil
}

func validatePinnedOptions(opts PinnedOptions) error {
	if !validRunID(opts.RunID) {
		return fmt.Errorf("worktree: pinned RunID %q must be a single path segment", opts.RunID)
	}
	if opts.RepoURL == "" || opts.BaseRef == "" {
		return fmt.Errorf("worktree: pinned RepoURL and BaseRef are required")
	}
	switch opts.CleanPolicy {
	case "", PinnedCleanNone, PinnedCleanIgnoredSafe, PinnedCleanFull:
		return nil
	default:
		return fmt.Errorf("worktree: unknown pinned clean policy %q", opts.CleanPolicy)
	}
}

func enqueuePinnedRun(root, runID string) (pinnedQueueEntry, error) {
	queue := pinnedQueueEntry{dir: filepath.Join(root, "lease.queue"), runID: runID}
	if err := os.MkdirAll(queue.dir, 0o755); err != nil {
		return queue, fmt.Errorf("worktree: create pinned lease queue: %w", err)
	}
	file, err := os.CreateTemp(queue.dir, fmt.Sprintf("%020d-%d-%s-", time.Now().UnixNano(), os.Getpid(), runID))
	if err != nil {
		return queue, fmt.Errorf("worktree: enqueue pinned run: %w", err)
	}
	queue.path = file.Name()
	pidStartedAt, _ := pinnedQueueProcessStartTime(os.Getpid())
	data, _ := json.Marshal(pinnedQueueRecord{RunID: runID, PID: os.Getpid(), PIDStartedAt: pidStartedAt})
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(queue.path)
		return queue, fmt.Errorf("worktree: persist pinned queue entry: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(queue.path)
		return queue, fmt.Errorf("worktree: close pinned queue entry: %w", err)
	}
	return queue, nil
}

func waitForPinnedLease(ctx context.Context, queue pinnedQueueEntry, lockPath, recordPath string, opts PinnedOptions) (*lock.Handle, error) {
	lastPosition := -1
	for {
		position, err := pinnedQueuePosition(queue)
		if err != nil {
			return nil, err
		}
		visiblePosition, err := pinnedVisiblePosition(position, recordPath)
		if err != nil {
			return nil, err
		}
		if visiblePosition != lastPosition && opts.OnQueuePosition != nil {
			if err := opts.OnQueuePosition(visiblePosition); err != nil {
				return nil, err
			}
			lastPosition = visiblePosition
		}
		if opts.OnQueueWait != nil {
			opts.OnQueueWait()
		}
		if position == 1 {
			handle, err := lock.TryAcquire(lockPath)
			if err == nil {
				return handle, nil
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
}

func pinnedQueuePosition(queue pinnedQueueEntry) (int, error) {
	entries, err := os.ReadDir(queue.dir)
	if err != nil {
		return 0, fmt.Errorf("worktree: read pinned lease queue: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(queue.dir, entry.Name())
		orphaned, err := pinnedQueueEntryOrphaned(path, entry.Name())
		if err != nil {
			return 0, err
		}
		if orphaned {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return 0, fmt.Errorf("worktree: remove orphaned pinned queue entry: %w", err)
			}
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	for i, name := range names {
		if filepath.Join(queue.dir, name) == queue.path {
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("worktree: pinned queue entry disappeared for run %s", queue.runID)
}

func pinnedVisiblePosition(position int, recordPath string) (int, error) {
	data, err := os.ReadFile(recordPath)
	if err == nil && len(strings.TrimSpace(string(data))) > 0 {
		return position + 1, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("worktree: read pinned lease position: %w", err)
	}
	return position, nil
}

// RecordOutcome updates the consecutive terminal-failure count while the
// caller still owns the workspace lease.
func (l *PinnedLease) RecordOutcome(failed bool) (int, error) {
	if l == nil || l.root == "" {
		return 0, nil
	}
	path := filepath.Join(l.root, "failure-streak.json")
	state := pinnedFailureState{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return 0, fmt.Errorf("worktree: decode pinned failure streak: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("worktree: read pinned failure streak: %w", err)
	}
	if failed {
		state.Count++
	} else {
		state.Count = 0
	}
	data, err := json.Marshal(state)
	if err != nil {
		return 0, fmt.Errorf("worktree: encode pinned failure streak: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return 0, fmt.Errorf("worktree: write pinned failure streak: %w", err)
	}
	return state.Count, nil
}

// ResetPinned exclusively tears down and immediately re-materializes a pinned
// workspace. A live run's lease makes the reset fail rather than race it.
func (m *Manager) ResetPinned(ctx context.Context, opts PinnedResetOptions) (string, error) {
	if opts.RepoURL == "" || opts.BaseRef == "" {
		return "", fmt.Errorf("worktree: pinned reset RepoURL and BaseRef are required")
	}
	key := repoKey(opts.RepoURL)
	root := filepath.Join(m.pinnedRoot, key)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("worktree: create pinned reset root: %w", err)
	}
	handle, err := lock.TryAcquire(filepath.Join(root, "pin.lock"))
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			return "", fmt.Errorf("worktree: pinned workspace is leased by a live run")
		}
		return "", fmt.Errorf("worktree: acquire pinned reset lock: %w", err)
	}
	defer func() { _ = handle.Release() }()

	pinDir := filepath.Join(root, "pin")
	if err := m.pinnedProcessKiller(pinDir); err != nil {
		return "", fmt.Errorf("worktree: terminate pinned workspace processes: %w", err)
	}
	if err := m.handoffPinnedState(ctx, key, ""); err != nil {
		return "", err
	}
	if err := m.handoffPinnedReceipts(ctx, key); err != nil {
		return "", err
	}
	if err := os.RemoveAll(pinDir); err != nil {
		return "", fmt.Errorf("worktree: remove pinned workspace: %w", err)
	}
	for _, path := range []string{
		filepath.Join(root, "pin.lease.json"),
		filepath.Join(root, "failure-streak.json"),
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("worktree: clear pinned reset state: %w", err)
		}
	}
	workspace, err := m.preparePinnedWithCustody(ctx, key, PinnedOptions{
		RepoURL: opts.RepoURL,
		RunID:   "workspace-reset",
		BaseRef: opts.BaseRef,
	})
	if err != nil {
		_ = os.RemoveAll(pinDir)
		return "", fmt.Errorf("worktree: re-materialize pinned workspace: %w", err)
	}
	return workspace.Path, nil
}

func pinnedQueueEntryOrphaned(path, name string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, fmt.Errorf("worktree: read pinned queue entry: %w", err)
	}
	var record pinnedQueueRecord
	if err := json.Unmarshal(data, &record); err != nil {
		parts := strings.SplitN(name, "-", 3)
		if len(parts) < 3 {
			return false, fmt.Errorf("worktree: decode pinned queue entry %q: %w", name, err)
		}
		record.PID, err = strconv.Atoi(parts[1])
		if err != nil {
			return false, fmt.Errorf("worktree: decode pinned queue entry %q: %w", name, err)
		}
	}
	if !pinnedQueueProcessAlive(record.PID) {
		return true, nil
	}
	if record.PIDStartedAt.IsZero() {
		return false, nil
	}
	startedAt, ok := pinnedQueueProcessStartTime(record.PID)
	if !ok {
		return false, nil
	}
	difference := startedAt.Sub(record.PIDStartedAt)
	if difference < 0 {
		difference = -difference
	}
	return difference > pidReusedTolerance, nil
}

func (m *Manager) preparePinned(ctx context.Context, key string, opts PinnedOptions) (*Worktree, error) {
	if err := m.handoffPinnedReceipts(ctx, key); err != nil {
		return nil, err
	}
	root := filepath.Join(m.pinnedRoot, key)
	repoDir := filepath.Join(root, "repo.git")
	pinDir := filepath.Join(root, "pin")
	if err := ensurePinnedMirror(ctx, repoDir, opts.RepoURL); err != nil {
		return nil, err
	}
	refspecs := []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}
	if err := m.runRemoteGit(ctx, opts.RepoURL, repoDir, append([]string{"fetch", "--prune", "origin"}, refspecs...)...); err != nil {
		return nil, fmt.Errorf("worktree: fetch pinned mirror: %w", err)
	}
	if err := ensureManagedGitConfig(ctx, repoDir); err != nil {
		return nil, err
	}
	if err := m.preflightPinnedPathLength(ctx, repoDir, pinDir, opts); err != nil {
		return nil, err
	}
	createdPin, err := ensurePinnedWorkspace(ctx, root, repoDir, pinDir, opts.RepoURL)
	if err != nil {
		return nil, err
	}
	if !createdPin {
		if err := fetchPinnedWorkspaceRefs(ctx, pinDir); err != nil {
			return nil, fmt.Errorf("worktree: refresh pinned workspace refs: %w", err)
		}
	}
	baseRef := pinnedBaseRef(ctx, pinDir, opts.BaseRef)
	if err := runGit(ctx, pinDir, "checkout", "--detach", "--force", baseRef); err != nil {
		return nil, fmt.Errorf("worktree: reset pinned workspace to base: %w", err)
	}
	if err := runGit(ctx, pinDir, "reset", "--hard", baseRef); err != nil {
		return nil, fmt.Errorf("worktree: reset pinned workspace: %w", err)
	}
	existing, err := checkoutPinnedBranch(ctx, pinDir, baseRef, opts)
	if err != nil {
		return nil, err
	}
	if err := cleanPinnedWorkspace(ctx, pinDir, opts.CleanPolicy); err != nil {
		return nil, err
	}
	if err := runGit(ctx, pinDir, "config", "user.name", BotGitUserName); err != nil {
		return nil, err
	}
	if err := runGit(ctx, pinDir, "config", "user.email", BotGitUserEmail); err != nil {
		return nil, err
	}
	if err := EnsureScratchExcluded(ctx, pinDir); err != nil {
		return nil, err
	}
	startRef, err := finishPinnedPreparation(ctx, pinDir, opts, existing)
	if err != nil {
		return nil, err
	}
	if opts.RunID != "workspace-reset" {
		if err := writeMarkerData(filepath.Join(root, pinnedReceiptOwnerFile), []byte(opts.RunID)); err != nil {
			return nil, err
		}
	}
	return &Worktree{
		RunID: opts.RunID, Path: pinDir, Branch: opts.Branch, PinnedWorkspaceCreated: createdPin,
		manager: m, key: key, startRef: startRef, repoURL: opts.RepoURL,
		pinned: true, repoDir: pinDir,
	}, nil
}

func ensurePinnedMirror(ctx context.Context, repoDir, repoURL string) error {
	if _, err := os.Stat(repoDir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("worktree: stat pinned mirror: %w", err)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return fmt.Errorf("worktree: create pinned mirror: %w", err)
	}
	if err := runGit(ctx, repoDir, "init", "--bare"); err != nil {
		return fmt.Errorf("worktree: initialize pinned mirror: %w", err)
	}
	if err := runGit(ctx, repoDir, "remote", "add", "origin", repoURL); err != nil {
		return fmt.Errorf("worktree: configure pinned mirror origin: %w", err)
	}
	return nil
}

func (m *Manager) preflightPinnedPathLength(ctx context.Context, repoDir, pinDir string, opts PinnedOptions) error {
	limit, ok := m.pathLengthLimit(opts.RepoURL)
	if !ok {
		return nil
	}
	refs := []string{opts.BaseRef}
	if opts.Branch != "" && branchExists(ctx, repoDir, opts.Branch) {
		refs[0] = opts.Branch
		if opts.SyncBase {
			refs = append(refs, opts.BaseRef)
		}
	}
	for _, ref := range refs {
		if err := preflightPathLength(ctx, repoDir, ref, pinDir, limit); err != nil {
			return err
		}
	}
	return nil
}

func ensurePinnedWorkspace(ctx context.Context, root, repoDir, pinDir, repoURL string) (bool, error) {
	if _, err := os.Stat(pinDir); err == nil {
		if err := ensureManagedGitConfig(ctx, pinDir); err != nil {
			return false, err
		}
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("worktree: stat pinned workspace: %w", err)
	}
	if err := runGit(ctx, root, "clone", "--no-checkout", repoDir, pinDir); err != nil {
		return false, fmt.Errorf("worktree: materialize pinned workspace: %w", err)
	}
	if err := ensureManagedGitConfig(ctx, pinDir); err != nil {
		return false, err
	}
	if err := runGit(ctx, pinDir, "remote", "rename", "origin", "mirror"); err != nil {
		return false, fmt.Errorf("worktree: name pinned mirror remote: %w", err)
	}
	if err := runGit(ctx, pinDir, "remote", "add", "origin", repoURL); err != nil {
		return false, fmt.Errorf("worktree: configure pinned push remote: %w", err)
	}
	return true, nil
}

func checkoutPinnedBranch(ctx context.Context, pinDir, baseRef string, opts PinnedOptions) (bool, error) {
	existing := false
	var err error
	if opts.Branch != "" {
		existing, err = ensurePinnedBranch(ctx, pinDir, opts.Branch)
		if err != nil {
			return false, err
		}
	}
	switch {
	case opts.Branch == "":
		err = runGit(ctx, pinDir, "checkout", "--detach", "--force", baseRef)
		if err != nil {
			err = fmt.Errorf("worktree: checkout pinned base: %w", err)
		}
	case existing:
		err = runGit(ctx, pinDir, "checkout", "--force", opts.Branch)
		if err != nil {
			err = fmt.Errorf("worktree: checkout pinned branch: %w", err)
		}
	case opts.RequireExistingBranch:
		err = fmt.Errorf("worktree: branch %q does not exist in pinned workspace for run %s", opts.Branch, opts.RunID)
	default:
		err = runGit(ctx, pinDir, "checkout", "-b", opts.Branch, baseRef)
		if err != nil {
			err = fmt.Errorf("worktree: create pinned branch: %w", err)
		}
	}
	return existing, err
}

func cleanPinnedWorkspace(ctx context.Context, pinDir string, policy PinnedCleanPolicy) error {
	switch policy {
	case PinnedCleanIgnoredSafe:
		if err := runGit(ctx, pinDir, "clean", "-ffd"); err != nil {
			return fmt.Errorf("worktree: clean pinned workspace: %w", err)
		}
	case PinnedCleanFull:
		if err := runGit(ctx, pinDir, "clean", "-ffdx"); err != nil {
			return fmt.Errorf("worktree: fully clean pinned workspace: %w", err)
		}
	}
	return nil
}

func finishPinnedPreparation(ctx context.Context, pinDir string, opts PinnedOptions, existing bool) (string, error) {
	startRef, err := gitOutput(ctx, pinDir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if opts.SyncBase && existing {
		baseRef := pinnedBaseRef(ctx, pinDir, opts.BaseRef)
		if err := runGit(ctx, pinDir, "merge", "--ff", "--no-edit", baseRef); err != nil {
			return "", fmt.Errorf("worktree: sync pinned branch %q with base %q: %w", opts.Branch, opts.BaseRef, err)
		}
	}
	return startRef, nil
}

// PreparePinned selects the branch and optional base synchronization requested
// by a stage after the caller has serialized access to the pinned workspace.
func (wt *Worktree) PreparePinned(ctx context.Context, opts PinnedPrepareOptions) error {
	if !wt.pinned {
		return fmt.Errorf("worktree: prepare pinned called for non-pinned workspace")
	}
	if opts.BaseRef == "" || opts.Branch == "" {
		return fmt.Errorf("worktree: pinned stage BaseRef and Branch are required")
	}
	if err := wt.manager.handoffPinnedState(ctx, wt.key, wt.RunID); err != nil {
		return err
	}
	repoDir := filepath.Join(wt.manager.pinnedRoot, wt.key, "repo.git")
	refspecs := []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}
	if err := wt.manager.runRemoteGit(ctx, wt.repoURL, repoDir, append([]string{"fetch", "--prune", "origin"}, refspecs...)...); err != nil {
		return fmt.Errorf("worktree: refresh pinned mirror: %w", err)
	}
	if err := fetchPinnedWorkspaceRefs(ctx, wt.Path); err != nil {
		return fmt.Errorf("worktree: refresh pinned workspace refs: %w", err)
	}
	baseRef := pinnedBaseRef(ctx, wt.Path, opts.BaseRef)
	existing, err := ensurePinnedBranch(ctx, wt.Path, opts.Branch)
	if err != nil {
		return err
	}
	switch {
	case existing:
		if err := runGit(ctx, wt.Path, "checkout", "--force", opts.Branch); err != nil {
			return fmt.Errorf("worktree: checkout pinned branch: %w", err)
		}
	case opts.RequireExistingBranch:
		return fmt.Errorf("worktree: branch %q does not exist in pinned workspace for run %s", opts.Branch, wt.RunID)
	default:
		if err := runGit(ctx, wt.Path, "checkout", "-b", opts.Branch, baseRef); err != nil {
			return fmt.Errorf("worktree: create pinned branch: %w", err)
		}
	}
	if err := runGit(ctx, wt.Path, "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("worktree: reset pinned stage workspace: %w", err)
	}
	startRef, err := gitOutput(ctx, wt.Path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if opts.SyncBase && existing {
		if mergeErr := runGit(ctx, wt.Path, "merge", "--ff", "--no-edit", baseRef); mergeErr != nil {
			resolved, resolveErr := resolveBaseSyncConflict(ctx, wt.Path)
			if !resolved {
				if resolveErr != nil {
					mergeErr = errors.Join(mergeErr, resolveErr)
				}
				conflictingFiles, inspectErr := mergeConflictFiles(ctx, wt.Path)
				cleanupErr := runGit(context.WithoutCancel(ctx), wt.Path, "merge", "--abort")
				return baseSyncFailure(CreateOptions{
					RunID: wt.RunID, BaseRef: opts.BaseRef, Branch: opts.Branch,
				}, mergeErr, conflictingFiles, inspectErr, cleanupErr)
			}
		}
	}
	wt.Branch = opts.Branch
	wt.startRef = startRef
	return nil
}

func fetchPinnedWorkspaceRefs(ctx context.Context, pinDir string) error {
	return runGit(ctx, pinDir, "fetch", "--prune", "mirror",
		"+refs/heads/*:refs/remotes/mirror/*",
		"+refs/tags/*:refs/tags/*")
}

func pinnedBaseRef(ctx context.Context, pinDir, baseRef string) string {
	remoteRef := "refs/remotes/mirror/" + baseRef
	if refExists(ctx, pinDir, remoteRef) {
		return remoteRef
	}
	return baseRef
}

func ensurePinnedBranch(ctx context.Context, pinDir, branch string) (bool, error) {
	if branchExists(ctx, pinDir, branch) {
		return true, nil
	}
	remoteRef := "refs/remotes/mirror/" + branch
	if !refExists(ctx, pinDir, remoteRef) {
		return false, nil
	}
	if err := runGit(ctx, pinDir, "branch", branch, remoteRef); err != nil {
		return false, fmt.Errorf("worktree: create local branch %q from pinned mirror: %w", branch, err)
	}
	return true, nil
}

func refExists(ctx context.Context, repoDir, ref string) bool {
	_, err := gitOutput(ctx, repoDir, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}
