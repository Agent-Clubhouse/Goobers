package childworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type childDispositionIntent struct {
	Version     int                        `json:"version"`
	Identity    triggerqueue.ChildIdentity `json:"identity"`
	Action      string                     `json:"action"`
	ResultRef   string                     `json:"resultRef"`
	Application *recovery.ChildApplyPlan   `json:"application,omitempty"`
}

// RequestDisposition accepts a parent's choice for its own terminal child.
// This asks the runner to yield; it never edits a live harness workspace.
func (s *SubmissionService) RequestDisposition(ctx context.Context, origin Origin, key, action, resultRef string) (triggerqueue.ChildDisposition, error) {
	submission, err := s.Get(ctx, origin, key)
	if err != nil {
		return triggerqueue.ChildDisposition{}, err
	}
	if action != "merge" && action != "replace" && action != "discard" {
		return triggerqueue.ChildDisposition{}, ErrSubmissionInvalid
	}
	binding, err := s.boundAuthority(ctx, origin)
	if err != nil {
		return triggerqueue.ChildDisposition{}, err
	}
	return s.Queue.RequestChildDisposition(ctx, triggerqueue.ChildDispositionRequest{Identity: submission.Child.Identity, Action: action, ResultRef: resultRef, Authority: binding}, s.now())
}

// ApplyDisposition runs only after the trusted runner has stopped and joined
// parent writers and holds exclusive custody. It persists intent before files,
// resumes the exact intent after interruption, verifies effects, then releases
// the child slot atomically. The host checks current policy before this call.
// It never retracts provider-side effects such as a published child PR.
func (c *WorkspaceCoordinator) ApplyDisposition(ctx context.Context, child triggerqueue.ChildRecord, parent *YieldedWorkspace, now time.Time) (TerminalResult, error) {
	repoURL := ""
	if parent != nil {
		repoURL = parent.RepoURL
	}
	result, err := c.ReadResult(ctx, child, repoURL)
	if err != nil {
		return TerminalResult{}, err
	}
	request, err := c.Queue.ChildDisposition(ctx, child.Identity)
	if err != nil {
		return TerminalResult{}, err
	}
	if request.ResultRef != result.ResultRef {
		return TerminalResult{}, triggerqueue.ErrConflict
	}
	if !request.AppliedAt.IsZero() {
		return result, nil
	}
	if len(request.Plan) == 0 {
		intent, err := c.planDisposition(ctx, child, request, result, parent)
		if err != nil {
			return TerminalResult{}, err
		}
		data, err := json.Marshal(intent)
		if err != nil {
			return TerminalResult{}, err
		}
		if err := c.Queue.KeepChildDispositionPlan(ctx, request, data); err != nil {
			return TerminalResult{}, err
		}
		request, err = c.Queue.ChildDisposition(ctx, child.Identity)
		if err != nil {
			return TerminalResult{}, err
		}
	}
	intent, err := decodeDispositionIntent(child, request)
	if err != nil {
		return TerminalResult{}, err
	}
	if intent.Application != nil {
		if parent == nil || parent.Path == "" {
			return TerminalResult{}, ErrAuthorityUnavailable
		}
		if err := recovery.ApplyChildApplication(ctx, parent.Path, *intent.Application); err != nil {
			return TerminalResult{}, err
		}
	}
	if err := c.Queue.CompleteChildDisposition(ctx, request, now); err != nil {
		return TerminalResult{}, err
	}
	return result, nil
}

func decodeDispositionIntent(child triggerqueue.ChildRecord, request triggerqueue.ChildDisposition) (childDispositionIntent, error) {
	var intent childDispositionIntent
	if err := json.Unmarshal(request.Plan, &intent); err != nil {
		return intent, err
	}
	canonical, err := json.Marshal(intent)
	if err != nil || !bytes.Equal(canonical, request.Plan) || intent.Version != 1 || intent.Identity != child.Identity || intent.Action != request.Action || intent.ResultRef != request.ResultRef {
		return intent, triggerqueue.ErrConflict
	}
	return intent, nil
}

func (c *WorkspaceCoordinator) planDisposition(ctx context.Context, child triggerqueue.ChildRecord, request triggerqueue.ChildDisposition, result TerminalResult, parent *YieldedWorkspace) (childDispositionIntent, error) {
	intent := childDispositionIntent{Version: 1, Identity: child.Identity, Action: request.Action, ResultRef: result.ResultRef}
	if result.Snapshot == nil {
		return intent, nil
	}
	if parent == nil || parent.Path == "" {
		return intent, ErrAuthorityUnavailable
	}
	fork, err := c.RetainedFork(ctx, child, parent.RepoURL)
	if err != nil {
		return intent, err
	}
	if parent.RepositoryKey != fork.Record.RepositoryKey || !sameSnapshotPolicy(parent.Policy, fork.Policy) {
		return intent, ErrAuthorityUnavailable
	}
	stored, err := c.Queue.ChildResult(ctx, child.Identity)
	if err != nil {
		return intent, err
	}
	if err := importChildCarrier(ctx, parent.Path, *result.Snapshot, stored.Bundle); err != nil {
		return intent, err
	}
	current, err := recovery.CaptureChildSnapshot(ctx, parent.Path, parent.RepositoryKey, child.Identity.ParentRunID, request.RequestedAt, request.RequestedAt.Add(triggerqueue.ChildRetention), fork.Policy)
	if err != nil {
		return intent, err
	}
	prepared, err := recovery.PrepareChildDisposition(ctx, parent.Path, fork, current, result.Snapshot.Record, recovery.ChildDisposition(request.Action), "child-disposition-"+child.RunID, request.RequestedAt, triggerqueue.MaxChildSnapshotBytes)
	if err != nil {
		return intent, err
	}
	plan, err := recovery.PlanChildApplication(ctx, parent.Path, prepared)
	if err != nil {
		return intent, fmt.Errorf("prepare child application: %w", err)
	}
	intent.Application = &plan
	return intent, nil
}
