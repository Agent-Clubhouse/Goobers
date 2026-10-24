package childworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

// TerminalResultInput retains the runner API and JSON shape while sharing the
// bounded wire contract with engine child continuations.
type TerminalResultInput = childresult.TerminalResultInput

// TerminalResult identifies retained result custody, not an applied parent
// disposition. The occurrence remains occupied until application is verified.
type TerminalResult struct {
	Input        TerminalResultInput
	ResultRef    string
	WorkspaceRef string
	Snapshot     *recovery.ChildSnapshot
}

type terminalResultReceipt struct {
	Version          int                        `json:"version"`
	Identity         triggerqueue.ChildIdentity `json:"identity"`
	AcceptanceID     string                     `json:"acceptanceId"`
	SourceDigest     string                     `json:"sourceDigest"`
	RunID            string                     `json:"runId"`
	Input            TerminalResultInput        `json:"input"`
	RepositoryDigest string                     `json:"repositoryDigest,omitempty"`
	Snapshot         *recovery.ChildSnapshot    `json:"snapshot,omitempty"`
}

func validTerminalInput(input TerminalResultInput, child triggerqueue.ChildRecord) bool {
	return input.Validate(child.AcceptedAt) == nil
}

// CaptureResult retains first terminal observation and optional isolated
// workspace. A nil workspace is valid for scratch runs or pre-launch rejection.
// It cannot replace a prior result with changed content on retry.
func (c *WorkspaceCoordinator) CaptureResult(ctx context.Context, child triggerqueue.ChildRecord, workspace *YieldedWorkspace, input TerminalResultInput) (TerminalResult, error) {
	if c == nil || c.Queue == nil || !validTerminalInput(input, child) {
		return TerminalResult{}, ErrAuthorityUnavailable
	}
	repoURL := ""
	if workspace != nil {
		repoURL = workspace.RepoURL
	}
	retained, err := c.ReadResult(ctx, child, repoURL)
	if err == nil {
		if retained.Input.State != input.State || !retained.Input.FinishedAt.Equal(input.FinishedAt) || retained.Input.Summary != input.Summary || !slices.Equal(retained.Input.References, input.References) || (retained.Snapshot != nil) != (workspace != nil) {
			return TerminalResult{}, triggerqueue.ErrConflict
		}
		return retained, nil
	}
	if !errors.Is(err, triggerqueue.ErrChildResultPending) {
		return TerminalResult{}, err
	}
	receipt := terminalResultReceipt{Version: 1, Identity: child.Identity, AcceptanceID: child.AcceptanceID, SourceDigest: child.ProposalDigest, RunID: child.RunID, Input: input}
	var carrier bytes.Buffer
	if workspace != nil {
		snapshot, err := c.captureResultWorkspace(ctx, child, *workspace, input.FinishedAt, &carrier)
		if err != nil {
			return TerminalResult{}, err
		}
		receipt.Snapshot = &snapshot
		receipt.RepositoryDigest = worktree.RepositoryDigest(repoURL)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return TerminalResult{}, err
	}
	stored := triggerqueue.ChildResult{Receipt: data, ReceiptDigest: journal.Digest(data), Bundle: carrier.Bytes()}
	if receipt.Snapshot != nil {
		stored.BundleDigest = receipt.Snapshot.Record.ArchiveDigest
	}
	if err := c.Queue.KeepChildResult(ctx, child, stored); err != nil {
		return TerminalResult{}, err
	}
	return c.ReadResult(ctx, child, repoURL)
}

func (c *WorkspaceCoordinator) captureResultWorkspace(ctx context.Context, child triggerqueue.ChildRecord, workspace YieldedWorkspace, finished time.Time, carrier *bytes.Buffer) (recovery.ChildSnapshot, error) {
	if workspace.Path == "" || workspace.RepoURL == "" {
		return recovery.ChildSnapshot{}, ErrAuthorityUnavailable
	}
	stored, err := c.Queue.ChildSnapshot(ctx, child.Identity)
	if err != nil {
		return recovery.ChildSnapshot{}, err
	}
	fork, err := decodeChildSnapshot(child, workspace.RepoURL, stored)
	if err != nil {
		return recovery.ChildSnapshot{}, err
	}
	if workspace.RepositoryKey != fork.Snapshot.Record.RepositoryKey || !sameSnapshotPolicy(workspace.Policy, fork.Snapshot.Policy) {
		return recovery.ChildSnapshot{}, ErrAuthorityUnavailable
	}
	snapshot, err := recovery.CaptureChildResult(ctx, workspace.Path, child.RunID, fork.Snapshot, finished, finished.Add(triggerqueue.ChildRetention))
	if err != nil {
		return recovery.ChildSnapshot{}, err
	}
	return recovery.WriteChildSnapshotBundle(ctx, workspace.Path, snapshot, carrier, triggerqueue.MaxChildSnapshotBytes)
}

func sameSnapshotPolicy(a, b recovery.SnapshotPolicy) bool {
	x, y := slices.Clone(a.ExcludedPaths), slices.Clone(b.ExcludedPaths)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// ReadResult verifies result identity, original fork, carrier and source pins.
// It does not grant permission to apply the returned snapshot to a parent.
func (c *WorkspaceCoordinator) ReadResult(ctx context.Context, child triggerqueue.ChildRecord, repoURL string) (TerminalResult, error) {
	if c == nil || c.Queue == nil {
		return TerminalResult{}, ErrAuthorityUnavailable
	}
	result, receipt, stored, err := readStoredResult(ctx, c.Queue, child)
	if err != nil {
		return TerminalResult{}, err
	}
	if receipt.Snapshot == nil {
		if len(stored.Bundle) != 0 || receipt.RepositoryDigest != "" {
			return TerminalResult{}, triggerqueue.ErrChildResultUnavailable
		}
		return result, nil
	}
	if err := c.verifyResultSnapshot(ctx, child, repoURL, receipt, stored); err != nil {
		return TerminalResult{}, err
	}
	result.WorkspaceRef = receipt.Snapshot.Record.SnapshotSHA
	return result, nil
}

func (c *WorkspaceCoordinator) verifyResultSnapshot(ctx context.Context, child triggerqueue.ChildRecord, repoURL string, receipt terminalResultReceipt, stored triggerqueue.ChildResult) error {
	if repoURL == "" || receipt.RepositoryDigest != worktree.RepositoryDigest(repoURL) {
		return triggerqueue.ErrChildResultUnavailable
	}
	forkBytes, err := c.Queue.ChildSnapshot(ctx, child.Identity)
	if err != nil {
		return err
	}
	fork, err := decodeChildSnapshot(child, repoURL, forkBytes)
	if err != nil {
		return err
	}
	snapshot := receipt.Snapshot
	record := snapshot.Record
	if record.RunID != child.RunID || !record.CreatedAt.Equal(receipt.Input.FinishedAt) || record.BaseSHA != fork.Snapshot.Record.SnapshotSHA || record.BaseRef != record.BaseSHA || record.RepositoryKey != fork.Snapshot.Record.RepositoryKey || record.ArchiveDigest != stored.BundleDigest || record.ArchiveBytes != int64(len(stored.Bundle)) || !reflect.DeepEqual(snapshot.Policy, fork.Snapshot.Policy) {
		return triggerqueue.ErrChildResultUnavailable
	}
	return record.Validate()
}

// ResultCustody is the custody a workspace-less terminal result needs. The
// local and the cloud child stores both implement it.
type ResultCustody interface {
	GetChild(ctx context.Context, identity triggerqueue.ChildIdentity) (triggerqueue.ChildRecord, error)
	ChildResult(ctx context.Context, identity triggerqueue.ChildIdentity) (triggerqueue.ChildResult, error)
	KeepChildResult(ctx context.Context, child triggerqueue.ChildRecord, result triggerqueue.ChildResult) error
}

// readStoredResult verifies the retained result receipt's identity, canonical
// encoding and bounds against the child and its current state.
func readStoredResult(ctx context.Context, custody ResultCustody, child triggerqueue.ChildRecord) (TerminalResult, terminalResultReceipt, triggerqueue.ChildResult, error) {
	current, err := custody.GetChild(ctx, child.Identity)
	if err != nil {
		return TerminalResult{}, terminalResultReceipt{}, triggerqueue.ChildResult{}, err
	}
	if current.AcceptanceID != child.AcceptanceID || current.ProposalDigest != child.ProposalDigest || current.RunID != child.RunID {
		return TerminalResult{}, terminalResultReceipt{}, triggerqueue.ChildResult{}, ErrAuthorityUnavailable
	}
	stored, err := custody.ChildResult(ctx, child.Identity)
	if err != nil {
		return TerminalResult{}, terminalResultReceipt{}, triggerqueue.ChildResult{}, err
	}
	var receipt terminalResultReceipt
	if err := json.Unmarshal(stored.Receipt, &receipt); err != nil {
		return TerminalResult{}, terminalResultReceipt{}, triggerqueue.ChildResult{}, triggerqueue.ErrChildResultUnavailable
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(canonical, stored.Receipt) || receipt.Version != 1 || receipt.Identity != child.Identity || receipt.AcceptanceID != child.AcceptanceID || receipt.SourceDigest != child.ProposalDigest || receipt.RunID != child.RunID || !validTerminalInput(receipt.Input, child) {
		return TerminalResult{}, terminalResultReceipt{}, triggerqueue.ChildResult{}, triggerqueue.ErrChildResultUnavailable
	}
	result := TerminalResult{Input: receipt.Input, ResultRef: stored.ReceiptDigest, Snapshot: receipt.Snapshot}
	if current.State.Terminal() && (current.State != receipt.Input.State || current.ResultRef != result.ResultRef) {
		return TerminalResult{}, terminalResultReceipt{}, triggerqueue.ChildResult{}, triggerqueue.ErrChildResultUnavailable
	}
	return result, receipt, stored, nil
}

// ReadTerminalResult reads a workspace-less terminal result (an engine child,
// which returns a result but no workspace snapshot). A retained receipt that
// carries a workspace is refused: this path never writes one.
func ReadTerminalResult(ctx context.Context, custody ResultCustody, child triggerqueue.ChildRecord) (TerminalResult, error) {
	result, receipt, stored, err := readStoredResult(ctx, custody, child)
	if err != nil {
		return TerminalResult{}, err
	}
	if receipt.Snapshot != nil || len(stored.Bundle) != 0 || receipt.RepositoryDigest != "" {
		return TerminalResult{}, triggerqueue.ErrChildResultUnavailable
	}
	return result, nil
}

// CaptureTerminalResult retains a workspace-less child's first terminal
// observation, exactly as WorkspaceCoordinator.CaptureResult does with no
// workspace. A retry with the same input returns it; one with changed content
// conflicts.
func CaptureTerminalResult(ctx context.Context, custody ResultCustody, child triggerqueue.ChildRecord, input TerminalResultInput) (TerminalResult, error) {
	if custody == nil || !validTerminalInput(input, child) {
		return TerminalResult{}, ErrAuthorityUnavailable
	}
	retained, err := ReadTerminalResult(ctx, custody, child)
	if err == nil {
		if retained.Input.State != input.State || !retained.Input.FinishedAt.Equal(input.FinishedAt) || retained.Input.Summary != input.Summary || !slices.Equal(retained.Input.References, input.References) {
			return TerminalResult{}, triggerqueue.ErrConflict
		}
		return retained, nil
	}
	if !errors.Is(err, triggerqueue.ErrChildResultPending) {
		return TerminalResult{}, err
	}
	data, err := json.Marshal(terminalResultReceipt{Version: 1, Identity: child.Identity, AcceptanceID: child.AcceptanceID, SourceDigest: child.ProposalDigest, RunID: child.RunID, Input: input})
	if err != nil {
		return TerminalResult{}, err
	}
	if err := custody.KeepChildResult(ctx, child, triggerqueue.ChildResult{Receipt: data, ReceiptDigest: journal.Digest(data)}); err != nil {
		return TerminalResult{}, err
	}
	return ReadTerminalResult(ctx, custody, child)
}
