package worktree

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

const CleanupDispositionUnknownBase = "retained-unknown-base"

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
	// RepositoryDigest and CreatedAt are copied from the durable marker.
	// Empty values identify legacy metadata and must not be guessed.
	RepositoryDigest string
	CreatedAt        time.Time
}

func (m *Manager) prepareCleanup(ctx context.Context, path, worktreeID, ownerRunID string) error {
	return m.prepareCleanupTarget(ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: ownerRunID})
}

func (m *Manager) prepareMarkerCleanup(ctx context.Context, path, worktreeID string, mk marker) error {
	return m.prepareCleanupTarget(ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: mk.OwnerRunID, Gaggle: mk.Gaggle, BaseRef: mk.BaseRef, StartRef: mk.StartRef, RepositoryDigest: mk.RepositoryDigest, CreatedAt: mk.CreatedAt})
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
	return m.preparePreservedTarget(ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: mk.OwnerRunID, Gaggle: mk.Gaggle, BaseRef: mk.BaseRef, StartRef: mk.StartRef, RepositoryDigest: mk.RepositoryDigest, CreatedAt: mk.CreatedAt})
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
