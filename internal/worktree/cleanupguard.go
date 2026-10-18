package worktree

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrCleanupDeferred means a durable handoff has not completed. The source
// must remain intact; housekeeping may report a warning and try other targets,
// but direct removal/replacement must still fail.
var ErrCleanupDeferred = errors.New("worktree cleanup deferred pending durable handoff")

// ErrCleanupRetained means cleanup was permanently quarantined because
// destroying the target could lose data. The durable marker records the
// operator-visible disposition and removes the target from prompt retries.
var ErrCleanupRetained = errors.New("worktree cleanup retained for operator review")

// CleanupDispositionUnknownBase identifies a target retained because its
// historical marker has no trustworthy recovery base.
const CleanupDispositionUnknownBase = "retained-unknown-base"

// CleanupDispositionRetryExhausted identifies a cleanup-pending target
// quarantined after its prompt retry budget was exhausted.
const CleanupDispositionRetryExhausted = "retained-retry-exhausted"

// CleanupRetentionError requests a durable, non-retrying cleanup disposition.
type CleanupRetentionError struct {
	Disposition string
	Cause       error
}

func (e *CleanupRetentionError) Error() string {
	if e.Cause == nil {
		return e.Disposition
	}
	return fmt.Sprintf("%s: %v", e.Disposition, e.Cause)
}

func (e *CleanupRetentionError) Unwrap() error { return e.Cause }

// RetainCleanupTarget asks the manager to quarantine a target rather than
// repeatedly retrying a cleanup whose safety cannot be established.
func RetainCleanupTarget(disposition string, cause error) error {
	return &CleanupRetentionError{Disposition: disposition, Cause: cause}
}

// VerifyCleanupTargetUnchanged proves that a legacy target has neither
// advanced from its recorded creation commit nor accumulated tracked,
// untracked, or conflicted working-tree state.
func VerifyCleanupTargetUnchanged(ctx context.Context, target CleanupTarget) error {
	startRef := strings.TrimSpace(target.StartRef)
	if startRef == "" {
		return fmt.Errorf("worktree cleanup cannot prove an unchanged target without its starting ref")
	}
	head, err := runCleanupGitOutput(ctx, target.Path, "resolve cleanup HEAD", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("worktree cleanup cannot resolve HEAD: %w", err)
	}
	start, err := runCleanupGitOutput(ctx, target.Path, "resolve cleanup start", "rev-parse", "--verify", startRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("worktree cleanup cannot resolve starting ref: %w", err)
	}
	if head != start {
		return fmt.Errorf("worktree cleanup target advanced from its recorded starting ref")
	}
	status, err := runCleanupGitOutput(ctx, target.Path, "inspect cleanup status", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("worktree cleanup cannot inspect working-tree state: %w", err)
	}
	if status != "" {
		return fmt.Errorf("worktree cleanup target contains unretained changes")
	}
	return nil
}

// VerifyCleanupTargetEmptyWithoutHEAD proves that a checkout never reached a
// commit and has no tracked, staged, or untracked content to recover.
func VerifyCleanupTargetEmptyWithoutHEAD(ctx context.Context, target CleanupTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	status, err := runCleanupGitOutput(ctx, target.Path, "inspect cleanup status", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("worktree cleanup cannot inspect working-tree state: %w", err)
	}
	if status != "" {
		return fmt.Errorf("worktree cleanup target contains unretained changes")
	}
	if err := verifyCleanupTargetUnbornHEAD(ctx, target); err != nil {
		return err
	}
	return nil
}

func verifyCleanupTargetUnbornHEAD(ctx context.Context, target CleanupTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := runCleanupGitOutput(ctx, target.Path, "resolve cleanup HEAD", "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		return fmt.Errorf("worktree cleanup target has HEAD")
	} else if !isGitMissingRef(err) {
		return fmt.Errorf("worktree cleanup cannot resolve HEAD: %w", err)
	}
	branch, err := runCleanupGitOutput(ctx, target.Path, "resolve cleanup symbolic HEAD", "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return fmt.Errorf("worktree cleanup cannot resolve symbolic HEAD: %w", err)
	}
	if !strings.HasPrefix(branch, "refs/heads/") {
		return fmt.Errorf("worktree cleanup HEAD is not an unborn branch")
	}
	if _, err := runCleanupGitOutput(ctx, target.Path, "resolve cleanup unborn branch", "show-ref", "--verify", "--quiet", branch); err == nil {
		return fmt.Errorf("worktree cleanup symbolic HEAD already exists")
	} else if !isGitMissingRef(err) {
		return fmt.Errorf("worktree cleanup cannot verify unborn branch: %w", err)
	}
	return ctx.Err()
}

func isGitMissingRef(err error) bool {
	var gitErr *gitCommandError
	return errors.As(err, &gitErr) && gitErr.exitCode == 1 && strings.TrimSpace(string(gitErr.output)) == ""
}

// CleanupTarget identifies the directory about to be destroyed. OwnerRunID
// comes from its durable marker; an empty value must not be guessed from the
// worktree ID, since run IDs and stage names can both contain hyphens.
type CleanupTarget struct {
	Path       string
	WorktreeID string
	OwnerRunID string
	Gaggle     string
	// BaseRef is copied from durable workspace ownership. It is the base
	// selected for the owning run, not a default inferred at cleanup time.
	BaseRef string
	// StartRef is the exact HEAD observed immediately after workspace
	// creation. Legacy compatibility may use it only to prove that a clean
	// terminal target has not advanced since creation.
	StartRef string
	// Pinned identifies a managed clone whose base branches live under the
	// mirror remote, rather than the local branches of a linked worktree.
	Pinned bool
	// RetainOnCleanup identifies non-terminal source-preservation cleanup
	// paths that must publish recovery before the source can be reset.
	RetainOnCleanup bool
	// RepositoryDigest and CreatedAt are copied from the durable marker.
	// Empty values identify legacy metadata and must not be guessed.
	RepositoryDigest string
	CreatedAt        time.Time
}

// LinkedWorktreeRepository returns the shared repository holding the refs of
// the linked run worktree at path, or false when path is not a run worktree
// directory of this manager. A linked worktree's branches live in that shared
// repository, so they outlive the checkout directory itself (#5383).
func (m *Manager) LinkedWorktreeRepository(path string) (string, bool) {
	rel, err := filepath.Rel(m.Root, path)
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[1] != "runs" || !validRunID(parts[0]) || !validRunID(parts[2]) ||
		filepath.Join(m.runsDirForKey(parts[0]), parts[2]) != filepath.Clean(path) {
		return "", false
	}
	return m.repoDirForKey(parts[0]), true
}

func (m *Manager) prepareCleanup(ctx context.Context, path, worktreeID, ownerRunID string) error {
	return m.prepareCleanupTarget(ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: ownerRunID})
}

func (m *Manager) prepareMarkerCleanup(ctx context.Context, path, worktreeID string, mk marker) error {
	return m.prepareCleanupTarget(ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: mk.OwnerRunID, Gaggle: mk.Gaggle, BaseRef: mk.BaseRef, StartRef: mk.StartRef, RetainOnCleanup: mk.RetainOnCleanup, RepositoryDigest: mk.RepositoryDigest, CreatedAt: mk.CreatedAt})
}

func (m *Manager) prepareMarkerCleanupWithRetention(ctx context.Context, key, path, markerPath, worktreeID string, mk marker) error {
	err := m.prepareMarkerCleanup(ctx, path, worktreeID, mk)
	if err == nil {
		return nil
	}
	var retain *CleanupRetentionError
	if !errors.As(err, &retain) {
		return err
	}
	if retainErr := m.markCleanupRetained(key, path, markerPath, mk, retain.Disposition); retainErr != nil {
		return errors.Join(err, retainErr)
	}
	return fmt.Errorf("%w: %w: %w", ErrCleanupDeferred, ErrCleanupRetained, retain)
}

func (m *Manager) prepareMarkerExit(ctx context.Context, path, worktreeID string, mk marker, keep bool) error {
	if !keep {
		return m.prepareMarkerCleanup(ctx, path, worktreeID, mk)
	}
	return m.preparePreservedTarget(ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: mk.OwnerRunID, Gaggle: mk.Gaggle, BaseRef: mk.BaseRef, StartRef: mk.StartRef, RetainOnCleanup: mk.RetainOnCleanup, RepositoryDigest: mk.RepositoryDigest, CreatedAt: mk.CreatedAt})
}

func (m *Manager) prepareMarkerExitWithRetention(ctx context.Context, key, path, markerPath, worktreeID string, mk marker, keep bool) error {
	if keep {
		return m.prepareMarkerExit(ctx, path, worktreeID, mk, true)
	}
	return m.prepareMarkerCleanupWithRetention(ctx, key, path, markerPath, worktreeID, mk)
}

// Keeping or releasing a workspace may capture recovery, but does not retire
// its mutation sidecar. Receipt handoff waits until destructive cleanup/reuse.
func (m *Manager) preparePreservedTarget(ctx context.Context, target CleanupTarget) error {
	_, err := m.runCleanupGuards(ctx, target, false)
	return err
}

func (m *Manager) prepareCleanupTarget(ctx context.Context, target CleanupTarget) error {
	_, err := m.prepareCleanupTargetWithReceipts(ctx, target)
	return err
}

func (m *Manager) prepareCleanupTargetWithReceipts(ctx context.Context, target CleanupTarget) (bool, error) {
	return m.runCleanupGuards(ctx, target, true)
}

func (m *Manager) runCleanupGuards(ctx context.Context, target CleanupTarget, includeReceipts bool) (bool, error) {
	// Snapshot reloadable guards before invoking any callback. No registry
	// mutex is held during potentially slow archive/journal publication.
	m.cleanupGuardsMu.RLock()
	guards := maps.Clone(m.cleanupGuards)
	m.cleanupGuardsMu.RUnlock()
	receipts := false
	for _, name := range slices.Sorted(maps.Keys(guards)) {
		if name == MutationReceiptGuard && !includeReceipts {
			continue
		}
		if err := guards[name](ctx, target); err != nil {
			return false, fmt.Errorf("%w: %s handoff for %s: %w", ErrCleanupDeferred, name, target.WorktreeID, err)
		}
		receipts = receipts || name == MutationReceiptGuard
	}
	return receipts, nil
}

// MutationReceiptGuard identifies the handoff that authorizes retiring a
// mutation sidecar. An unrelated recovery handoff cannot acknowledge receipts.
const MutationReceiptGuard = "mutation-receipts"

// WithMutationReceiptCleanup installs the receipt-specific durable handoff.
func WithMutationReceiptCleanup(callback func(context.Context, CleanupTarget) error) ManagerOption {
	return func(m *Manager) {
		if callback != nil {
			_ = m.SetCleanupGuard(MutationReceiptGuard, callback)
		}
	}
}

// SetCleanupGuard installs one handoff without replacing other guards. Each
// cleanup snapshots the registry; callbacks run without its mutex held.
func (m *Manager) SetCleanupGuard(name string, callback func(context.Context, CleanupTarget) error) error {
	if name == "" || callback == nil {
		return fmt.Errorf("cleanup guard requires a name and callback")
	}
	m.cleanupGuardsMu.Lock()
	defer m.cleanupGuardsMu.Unlock()
	if m.cleanupGuards == nil {
		m.cleanupGuards = make(map[string]func(context.Context, CleanupTarget) error)
	}
	m.cleanupGuards[name] = callback
	return nil
}
