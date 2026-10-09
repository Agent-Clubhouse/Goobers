package childworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
)

// TerminalResultInput comes from the trusted execution observer after joining
// all writers. Summary is already redacted; references are bounded artifact
// identifiers, never instructions or authority. FinishedAt is durable journal
// or queue time; cancellation of an already escalated execution uses the later
// of its terminal event and the recorded family cancellation.
type TerminalResultInput struct {
	State      triggerqueue.ChildState `json:"state"`
	FinishedAt time.Time               `json:"finishedAt"`
	Summary    string                  `json:"summary,omitempty"`
	References []string                `json:"references,omitempty"`
}

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
	if !input.State.Terminal() || input.FinishedAt.IsZero() || input.FinishedAt.Before(child.AcceptedAt) || len(input.Summary) > 8192 || !utf8.ValidString(input.Summary) || strings.ContainsRune(input.Summary, 0) || len(input.References) > 16 {
		return false
	}
	for _, ref := range input.References {
		if !submissionText(ref, 1024) {
			return false
		}
	}
	return true
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
	current, err := c.Queue.GetChild(ctx, child.Identity)
	if err != nil {
		return TerminalResult{}, err
	}
	if current.AcceptanceID != child.AcceptanceID || current.ProposalDigest != child.ProposalDigest || current.RunID != child.RunID {
		return TerminalResult{}, ErrAuthorityUnavailable
	}
	stored, err := c.Queue.ChildResult(ctx, child.Identity)
	if err != nil {
		return TerminalResult{}, err
	}
	var receipt terminalResultReceipt
	if err := json.Unmarshal(stored.Receipt, &receipt); err != nil {
		return TerminalResult{}, triggerqueue.ErrChildResultUnavailable
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(canonical, stored.Receipt) || receipt.Version != 1 || receipt.Identity != child.Identity || receipt.AcceptanceID != child.AcceptanceID || receipt.SourceDigest != child.ProposalDigest || receipt.RunID != child.RunID || !validTerminalInput(receipt.Input, child) {
		return TerminalResult{}, triggerqueue.ErrChildResultUnavailable
	}
	result := TerminalResult{Input: receipt.Input, ResultRef: stored.ReceiptDigest, Snapshot: receipt.Snapshot}
	if current.State.Terminal() && (current.State != receipt.Input.State || current.ResultRef != result.ResultRef) {
		return TerminalResult{}, triggerqueue.ErrChildResultUnavailable
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
