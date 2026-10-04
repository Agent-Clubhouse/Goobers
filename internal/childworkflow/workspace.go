package childworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

const childWorkspaceCleanupTimeout = 10 * time.Second

// RetainedFork returns the accepted source policy and original fork identity.
// Trusted result/adoption callers use this instead of mutable current config.
func (c *WorkspaceCoordinator) RetainedFork(ctx context.Context, child triggerqueue.ChildRecord, repoURL string) (recovery.ChildSnapshot, error) {
	if c == nil || c.Queue == nil {
		return recovery.ChildSnapshot{}, ErrAuthorityUnavailable
	}
	current, err := c.Queue.GetChild(ctx, child.Identity)
	if err != nil {
		return recovery.ChildSnapshot{}, err
	}
	if current.AcceptanceID != child.AcceptanceID || current.ProposalDigest != child.ProposalDigest || current.RunID != child.RunID {
		return recovery.ChildSnapshot{}, ErrAuthorityUnavailable
	}
	stored, err := c.Queue.ChildSnapshot(ctx, child.Identity)
	if err != nil {
		return recovery.ChildSnapshot{}, err
	}
	receipt, err := decodeChildSnapshot(current, repoURL, stored)
	return receipt.Snapshot, err
}

// WorkspaceCoordinator owns the durable captured state for accepted children.
// The invocation owner, not a tool request, supplies acknowledged exclusive
// parent custody to Capture. Prepare consumes retained state without reading
// the current parent. Permission/placement checks remain the launcher's job.
type WorkspaceCoordinator struct {
	Queue     *triggerqueue.Store
	Worktrees *worktree.Manager
}

// YieldedWorkspace is supplied only by the trusted invocation owner after all
// harness subprocess writers have stopped and joined. The owner holds the
// workspace lease until Capture returns. No request or workflow may supply it.
type YieldedWorkspace struct {
	Path          string
	RepoURL       string
	RepositoryKey string
	Policy        recovery.SnapshotPolicy
}

type childSnapshotReceipt struct {
	Version          int                        `json:"version"`
	Identity         triggerqueue.ChildIdentity `json:"identity"`
	AcceptanceID     string                     `json:"acceptanceId"`
	SourceDigest     string                     `json:"sourceDigest"`
	RepositoryDigest string                     `json:"repositoryDigest"`
	Snapshot         recovery.ChildSnapshot     `json:"snapshot"`
}

// Capture persists a bounded snapshot while the trusted caller holds yielded
// workspace custody. Its stable time comes from acceptance, not a retry clock.
// Previously retained custody always wins; changed parent files are not read.
func (c *WorkspaceCoordinator) Capture(ctx context.Context, child triggerqueue.ChildRecord, parent YieldedWorkspace) error {
	if c == nil || c.Queue == nil || parent.Path == "" || parent.RepoURL == "" || !blobstore.ValidDigest(child.ProposalDigest) {
		return ErrAuthorityUnavailable
	}
	retained, err := c.Queue.ChildSnapshot(ctx, child.Identity)
	if err == nil {
		_, err = decodeChildSnapshot(child, parent.RepoURL, retained)
		return err
	}
	if !errors.Is(err, triggerqueue.ErrChildSnapshotPending) {
		return err
	}
	snapshot, err := recovery.CaptureChildSnapshot(ctx, parent.Path, parent.RepositoryKey, child.Identity.ParentRunID, child.AcceptedAt, child.AcceptedAt.Add(triggerqueue.ChildRetention), parent.Policy)
	if err != nil {
		return err
	}
	var carrier bytes.Buffer
	snapshot, err = recovery.WriteChildSnapshotBundle(ctx, parent.Path, snapshot, &carrier, triggerqueue.MaxChildSnapshotBytes)
	if err != nil {
		return err
	}
	receipt, err := json.Marshal(childSnapshotReceipt{Version: 1, Identity: child.Identity, AcceptanceID: child.AcceptanceID, SourceDigest: child.ProposalDigest, RepositoryDigest: worktree.RepositoryDigest(parent.RepoURL), Snapshot: snapshot})
	if err != nil {
		return err
	}
	return c.Queue.KeepChildSnapshot(ctx, child, triggerqueue.ChildSnapshot{Receipt: receipt, ReceiptDigest: journal.Digest(receipt), Bundle: carrier.Bytes(), BundleDigest: snapshot.Record.ArchiveDigest})
}

// Prepare imports only the retained, verified carrier and creates or reuses its
// isolated managed fork. A missing snapshot is an explicit no-start deferral;
// changed/missing retained data is custody loss. An existing run journal must
// use Runner.Resume, whose adoption verifier never recreates its workspace.
func (c *WorkspaceCoordinator) Prepare(ctx context.Context, child triggerqueue.ChildRecord, repoURL string) (admitted *runner.ChildWorkspaceAdmission, prepareErr error) {
	if c == nil || c.Queue == nil || c.Worktrees == nil || repoURL == "" {
		return nil, ErrAuthorityUnavailable
	}
	current, err := c.Queue.GetChild(ctx, child.Identity)
	if err != nil {
		return nil, err
	}
	if current.AcceptanceID != child.AcceptanceID || current.ProposalDigest != child.ProposalDigest || current.RunID != child.RunID || current.ExecutionEpoch != 0 || child.ExecutionEpoch != 0 || current.CancellationRequested || !current.TombstonedAt.IsZero() {
		return nil, ErrAuthorityUnavailable
	}
	retained, err := c.Queue.ChildSnapshot(ctx, child.Identity)
	if err != nil {
		return nil, err
	}
	receipt, err := decodeChildSnapshot(current, repoURL, retained)
	if err != nil {
		return nil, err
	}
	return c.prepareExecutionFork(ctx, child, repoURL, receipt.Snapshot, retained.Bundle)
}

// prepareExecutionFork creates a distinct owned fork from verified durable
// bytes. A retry adopts the same directory without resetting prior progress.
func (c *WorkspaceCoordinator) prepareExecutionFork(ctx context.Context, child triggerqueue.ChildRecord, repoURL string, snapshot recovery.ChildSnapshot, bundle []byte) (admitted *runner.ChildWorkspaceAdmission, prepareErr error) {
	var mirror string
	// The database carrier owns recovery custody. The import pin is temporary;
	// the child branch protects the fork after creation. Always clean the exact
	// pin, including partial imports and failed checkouts, under the mirror lock.
	defer func() {
		if mirror == "" {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), childWorkspaceCleanupTimeout)
		defer cancel()
		_, err := c.Worktrees.WithExistingMirror(cleanup, repoURL, func(repository string) error {
			return recovery.DeleteSnapshotRef(cleanup, repository, snapshot.Record)
		})
		prepareErr = errors.Join(prepareErr, err)
	}()
	found, err := c.Worktrees.WithExistingMirror(ctx, repoURL, func(repository string) error {
		mirror = repository
		return importChildCarrier(ctx, repository, snapshot, bundle)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("child workspace base mirror is unavailable")
	}
	admission := &runner.ChildWorkspaceAdmission{WorkspaceID: child.ActiveRunID() + "-child", ForkSHA: snapshot.Record.SnapshotSHA, RepositoryDigest: worktree.RepositoryDigest(repoURL)}
	_, err = c.Worktrees.CreateChildFromSnapshot(ctx, worktree.ChildOptions{RepoURL: repoURL, RunID: admission.WorkspaceID, OwnerRunID: child.ActiveRunID(), Gaggle: child.Identity.Gaggle, SnapshotSHA: admission.ForkSHA})
	if err != nil {
		return nil, err
	}
	return admission, nil
}

func decodeChildSnapshot(child triggerqueue.ChildRecord, repoURL string, stored triggerqueue.ChildSnapshot) (childSnapshotReceipt, error) {
	var receipt childSnapshotReceipt
	if err := json.Unmarshal(stored.Receipt, &receipt); err != nil {
		return receipt, triggerqueue.ErrChildSnapshotUnavailable
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(canonical, stored.Receipt) {
		return receipt, triggerqueue.ErrChildSnapshotUnavailable
	}
	if receipt.Version != 1 || receipt.Identity != child.Identity || receipt.AcceptanceID != child.AcceptanceID || receipt.SourceDigest != child.ProposalDigest || receipt.RepositoryDigest != worktree.RepositoryDigest(repoURL) {
		return receipt, triggerqueue.ErrChildSnapshotUnavailable
	}
	record := receipt.Snapshot.Record
	if record.RunID != child.Identity.ParentRunID || !record.CreatedAt.Equal(child.AcceptedAt) || record.ArchiveDigest != stored.BundleDigest || record.ArchiveBytes != int64(len(stored.Bundle)) {
		return receipt, triggerqueue.ErrChildSnapshotUnavailable
	}
	if err := record.Validate(); err != nil {
		return receipt, err
	}
	if err := receipt.Snapshot.Policy.Validate(); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// One fixed temporary carrier per locked mirror bounds crash debris. The
// authoritative bytes remain in SQLite; this file is never a recovery selector.
func importChildCarrier(ctx context.Context, repository string, snapshot recovery.ChildSnapshot, data []byte) error {
	path := filepath.Join(repository, "goobers-child-import.bundle")
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("child snapshot staging is not a regular private file")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(path) }()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return recovery.ImportChildSnapshot(ctx, repository, path, snapshot, triggerqueue.MaxChildSnapshotBytes)
}
