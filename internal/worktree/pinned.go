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
	Worktree *Worktree
	handle   *lock.Handle
	queue    string
	record   string
	root     string
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
	pidStartedAt, _ := pinnedQueueProcessStartTime(os.Getpid())
	queueRecord, _ := json.Marshal(pinnedQueueRecord{
		RunID: opts.RunID, PID: os.Getpid(), PIDStartedAt: pidStartedAt,
	})
	if _, writeErr := queueFile.Write(queueRecord); writeErr != nil {
		_ = queueFile.Close()
		_ = os.Remove(queuePath)
		return nil, fmt.Errorf("worktree: persist pinned queue entry: %w", writeErr)
	}
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
			if entry.IsDir() {
				continue
			}
			orphaned, err := pinnedQueueEntryOrphaned(filepath.Join(queueDir, entry.Name()), entry.Name())
			if err != nil {
				return nil, err
			}
			if orphaned {
				if err := os.Remove(filepath.Join(queueDir, entry.Name())); err != nil && !os.IsNotExist(err) {
					return nil, fmt.Errorf("worktree: remove orphaned pinned queue entry: %w", err)
				}
				continue
			}
			names = append(names, entry.Name())
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
		if opts.OnQueueWait != nil {
			opts.OnQueueWait()
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

	lease := &PinnedLease{handle: handle, queue: queuePath, record: recordPath, root: root}
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
	workspace, err := m.preparePinned(ctx, key, PinnedOptions{
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
	if limit, ok := m.pathLengthLimit(opts.RepoURL); ok {
		refs := []string{opts.BaseRef}
		if opts.Branch != "" && branchExists(ctx, repoDir, opts.Branch) {
			refs[0] = opts.Branch
			if opts.SyncBase {
				refs = append(refs, opts.BaseRef)
			}
		}
		for _, ref := range refs {
			if err := preflightPathLength(ctx, repoDir, ref, pinDir, limit); err != nil {
				return nil, err
			}
		}
	}
	if _, err := os.Stat(pinDir); os.IsNotExist(err) {
		if err := runGit(ctx, root, "clone", "--no-checkout", repoDir, pinDir); err != nil {
			return nil, fmt.Errorf("worktree: materialize pinned workspace: %w", err)
		}
		// Unlike a linked `git worktree add` tree, `git clone` does not inherit
		// the source's custom config — pinDir starts with git's defaults, not
		// repoDir's managedGitConfig. In particular core.longpaths is unset
		// again here, so the very next operation (renaming the freshly cloned
		// "origin" remote, which rewrites every refs/remotes/origin/* ref) can
		// fail on Windows with ERROR_FILENAME_EXCED_RANGE for any ref whose
		// full on-disk path exceeds MAX_PATH — a real risk given the nested
		// pin/.git/refs/remotes/<remote>/<branch-namespace>/<runID> layout.
		// Apply the managed config before doing anything else in this tree.
		if err := ensureManagedGitConfig(ctx, pinDir); err != nil {
			return nil, err
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
	} else if err := ensureManagedGitConfig(ctx, pinDir); err != nil {
		// Self-heal a pin created before this policy existed, same rationale as
		// the mirror's own ensureManagedGitConfig call on every WorkingCopy.
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
	existing := false
	var err error
	if opts.Branch != "" {
		existing, err = ensurePinnedBranch(ctx, pinDir, opts.Branch)
		if err != nil {
			return nil, err
		}
	}
	switch {
	case opts.Branch == "":
		if err := runGit(ctx, pinDir, "checkout", "--detach", "--force", baseRef); err != nil {
			return nil, fmt.Errorf("worktree: checkout pinned base: %w", err)
		}
	case existing:
		if err := runGit(ctx, pinDir, "checkout", "--force", opts.Branch); err != nil {
			return nil, fmt.Errorf("worktree: checkout pinned branch: %w", err)
		}
	case opts.RequireExistingBranch:
		return nil, fmt.Errorf("worktree: branch %q does not exist in pinned workspace for run %s", opts.Branch, opts.RunID)
	default:
		if err := runGit(ctx, pinDir, "checkout", "-b", opts.Branch, baseRef); err != nil {
			return nil, fmt.Errorf("worktree: create pinned branch: %w", err)
		}
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
		if err := runGit(ctx, pinDir, "merge", "--ff", "--no-edit", baseRef); err != nil {
			return nil, fmt.Errorf("worktree: sync pinned branch %q with base %q: %w", opts.Branch, opts.BaseRef, err)
		}
	}
	return &Worktree{
		RunID: opts.RunID, Path: pinDir, Branch: opts.Branch, PinnedWorkspaceCreated: createdPin,
		manager: m, key: key, startRef: startRef, repoURL: opts.RepoURL,
		pinned: true, repoDir: pinDir,
	}, nil
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
		if err := runGit(ctx, wt.Path, "merge", "--ff", "--no-edit", baseRef); err != nil {
			conflictingFiles, inspectErr := mergeConflictFiles(ctx, wt.Path)
			cleanupErr := runGit(context.WithoutCancel(ctx), wt.Path, "merge", "--abort")
			return baseSyncFailure(CreateOptions{
				RunID: wt.RunID, BaseRef: opts.BaseRef, Branch: opts.Branch,
			}, err, conflictingFiles, inspectErr, cleanupErr)
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
