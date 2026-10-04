package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// daemonChildHandoff is installed in local runners at construction and resolves
// the daemon-owned services only when invoked, after startup wires the queue.
// It contains no model-supplied path, credentials, or cached permission grant.
type daemonChildHandoff struct {
	layout       instance.Layout
	worktrees    *worktree.Manager
	repoCloneURL func(apiv1.RepoRef) (string, error)
	project      apiv1.RepoRef
}

func (h *daemonChildHandoff) service() (*daemonCredentialService, error) {
	s, ok := stageGrantMinterFor(h.layout.Root).(*daemonCredentialService)
	if !ok || s.children == nil || s.childQueue == nil || s.childDispatch == nil {
		return nil, childworkflow.ErrAuthorityUnavailable
	}
	return s, nil
}

func (h *daemonChildHandoff) Await(ctx context.Context, env apiv1.InvocationEnvelope) (runner.ChildHandoffRequest, error) {
	if env.ChildWorkflowOrigin == nil || env.Gaggle != h.layout.Gaggle() {
		return runner.ChildHandoffRequest{}, childworkflow.ErrAuthorityUnavailable
	}
	s, err := h.service()
	if err != nil {
		return runner.ChildHandoffRequest{}, err
	}
	for {
		request, err := h.observe(ctx, s, env)
		if err != nil {
			if ctx.Err() != nil {
				return h.finalObservation(ctx, s, env, err)
			}
			return runner.ChildHandoffRequest{}, err
		}
		if request.RequestID != "" {
			return request, nil
		}
		if err := childHandoffPoll(ctx, 250*time.Millisecond); err != nil {
			return h.finalObservation(ctx, s, env, err)
		}
	}
}

// Invocation exit revokes its grant before this final bounded read. Acceptance
// committed before revocation must still be yielded, including a canceled read.
func (h *daemonChildHandoff) finalObservation(ctx context.Context, s *daemonCredentialService, env apiv1.InvocationEnvelope, cause error) (runner.ChildHandoffRequest, error) {
	final, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	request, err := h.observe(final, s, env)
	if err == nil && request.RequestID != "" {
		return request, nil
	}
	return runner.ChildHandoffRequest{}, errors.Join(cause, err)
}

func (h *daemonChildHandoff) observe(ctx context.Context, s *daemonCredentialService, env apiv1.InvocationEnvelope) (runner.ChildHandoffRequest, error) {
	child, err := s.childQueue.CurrentChild(ctx, triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}, env.ChildWorkflowOrigin.StageOccurrence)
	if err != nil || child.RunID == "" {
		return runner.ChildHandoffRequest{}, err
	}
	if child.CancellationRequested {
		return runner.ChildHandoffRequest{}, childworkflow.ErrAuthorityUnavailable
	}
	action, dispositionDigest := "wait", ""
	if child.State.Terminal() {
		request, err := s.childQueue.ChildDisposition(ctx, child.Identity)
		if errors.Is(err, triggerqueue.ErrChildDispositionPending) {
			return runner.ChildHandoffRequest{}, nil
		}
		if err != nil {
			return runner.ChildHandoffRequest{}, err
		}
		if !request.AppliedAt.IsZero() {
			return runner.ChildHandoffRequest{}, nil
		}
		if request.AttemptID != env.ChildWorkflowOrigin.AttemptID {
			// A new invocation must explicitly adopt/revise the old choice by
			// request-digest CAS before the host yields again.
			return runner.ChildHandoffRequest{}, nil
		}
		action, dispositionDigest = request.Action, request.RequestDigest()
	}
	request := runner.ChildHandoffRequest{Gaggle: env.Gaggle, ParentRunID: env.RunID, Action: action, DispositionDigest: dispositionDigest, ChildRunID: child.RunID, AcceptanceID: child.AcceptanceID, InvocationKey: child.Identity.InvocationKey, SourceDigest: child.ProposalDigest, Origin: *env.ChildWorkflowOrigin}
	request.RequestID = childHandoffRequestDigest(request)
	return request, nil
}

func childHandoffRequestDigest(request runner.ChildHandoffRequest) string {
	request.RequestID = ""
	data, _ := json.Marshal(request)
	return journal.Digest(data)
}

func (h *daemonChildHandoff) Yield(ctx context.Context, request runner.ChildHandoffRequest, custody runner.ChildWorkspaceCustody) error {
	s, err := h.service()
	if err != nil {
		return err
	}
	child, err := h.child(ctx, s, request)
	if err != nil {
		return err
	}
	id, stage, err := h.parkedParent(request)
	if err != nil {
		return err
	}
	pinned, release, err := s.children.AcquirePinnedStage(ctx, id, stage)
	if err != nil {
		return err
	}
	defer release()
	if pinned.ConfigGeneration != id.ConfigGeneration || pinned.WorkflowDigest != id.WorkflowDigest || pinned.GooberDigest != id.GooberDigest || childRepoKey(custody.RepoRef) != childRepoKey(pinned.Admission.Gaggle.Spec.Project) {
		return childworkflow.ErrAuthorityChanged
	}
	if err := h.validateAccepted(ctx, s, request, child, id, pinned); err != nil {
		return err
	}
	url, err := h.repoCloneURL(custody.RepoRef)
	if err != nil {
		return err
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: s.childQueue, Worktrees: h.worktrees}
	parent, err := h.yieldedWorkspace(ctx, s, &coordinator, child, request.Action, custody, url)
	if err != nil {
		return err
	}
	if request.Action == "wait" {
		return coordinator.Capture(ctx, child, parent)
	}
	disposition, err := s.childQueue.ChildDisposition(ctx, child.Identity)
	if err != nil {
		return err
	}
	if disposition.RequestDigest() != request.DispositionDigest || disposition.Action != request.Action || disposition.AttemptID != request.Origin.AttemptID {
		return childworkflow.ErrAuthorityChanged
	}
	_, err = coordinator.ApplyDisposition(ctx, child, &parent, time.Now().UTC())
	if err == nil {
		return nil
	}
	current, readErr := s.childQueue.ChildDisposition(ctx, child.Identity)
	if readErr != nil || current.RequestDigest() != disposition.RequestDigest() || !current.AppliedAt.IsZero() {
		return errors.Join(err, readErr)
	}
	if len(current.Plan) == 0 {
		return &runner.ChildDispositionWaitError{Reason: "Child disposition preparation was refused before parent files changed. Read the current disposition request digest and choose discard or a revised action."}
	}
	return &runner.ChildDispositionWaitError{Reconcile: true, Reason: "The published child application plan requires reconciliation. Parent writers remain stopped; another action cannot replace this plan."}
}

func (h *daemonChildHandoff) child(ctx context.Context, s *daemonCredentialService, request runner.ChildHandoffRequest) (triggerqueue.ChildRecord, error) {
	if request.Gaggle != h.layout.Gaggle() || request.RequestID != childHandoffRequestDigest(request) {
		return triggerqueue.ChildRecord{}, childworkflow.ErrAuthorityUnavailable
	}
	id := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: request.Gaggle, ParentRunID: request.ParentRunID}, StageOccurrence: request.Origin.StageOccurrence, InvocationKey: request.InvocationKey}
	child, err := s.childQueue.GetChild(ctx, id)
	if err != nil {
		return child, err
	}
	if child.RunID != request.ChildRunID || child.AcceptanceID != request.AcceptanceID || child.ProposalDigest != request.SourceDigest || child.CancellationRequested || !child.TombstonedAt.IsZero() {
		return child, childworkflow.ErrAuthorityChanged
	}
	return child, nil
}

func (h *daemonChildHandoff) parkedParent(request runner.ChildHandoffRequest) (journal.RunIdentity, string, error) {
	dir, err := h.layout.FindRunDir(request.ParentRunID)
	if err != nil {
		return journal.RunIdentity{}, "", err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return journal.RunIdentity{}, "", err
	}
	id, err := reader.Identity()
	if err != nil {
		return id, "", err
	}
	phase, err := reader.Phase()
	if err != nil || phase != journal.PhaseRunning || id.Gaggle != request.Gaggle || id.Child != nil {
		return id, "", childworkflow.ErrAuthorityUnavailable
	}
	events, err := reader.Events()
	if err != nil {
		return id, "", err
	}
	parked, started, waiting, err := runner.ParkedChildRequestForOrigin(events, request.Origin)
	if err != nil || !waiting || parked != request {
		return id, "", childworkflow.ErrAuthorityUnavailable
	}
	return id, started.Stage, nil
}

func (h *daemonChildHandoff) validateAccepted(ctx context.Context, s *daemonCredentialService, request runner.ChildHandoffRequest, child triggerqueue.ChildRecord, id journal.RunIdentity, pinned childworkflow.PinnedStageAdmission) error {
	actor := childworkflow.InvocationActor(id.RunID, request.Origin.StageOccurrence)
	receipt, err := s.childQueue.VerifiedChildStart(ctx, child.Identity, actor)
	if err != nil {
		return err
	}
	envelope, err := childworkflow.DecodeStartEnvelope(receipt.Payload)
	if err != nil {
		return err
	}
	source, err := s.childQueue.ChildProposal(ctx, child.Identity)
	if err != nil {
		return err
	}
	authority := childworkflow.Authority{Origin: childworkflow.Origin{Gaggle: id.Gaggle, RunID: id.RunID, StageOccurrence: request.Origin.StageOccurrence, ConfigDigest: pinned.Admission.ConfigDigest, PolicyDigest: childworkflow.AuthorityPolicyDigest(pinned.Admission)}, Actor: actor, Admission: pinned.Admission, ConfigGeneration: id.ConfigGeneration, ParentWorkflow: id.Workflow, ParentWorkflowDigest: id.WorkflowDigest, ParentGooberDigest: id.GooberDigest}
	if err := childworkflow.ValidateRetainedCustody(authority, envelope, child, source); err != nil {
		return err
	}
	if err := childworkflow.CheckDispositionAuthority(authority, request.Action); err != nil {
		return err
	}
	if request.Action != "discard" {
		_, err = childworkflow.ValidateRetainedStart(authority, envelope, source.Source)
	}
	return err
}

func (h *daemonChildHandoff) Wait(ctx context.Context, request runner.ChildHandoffRequest) (runner.ChildHandoffCompletion, error) {
	s, err := h.service()
	if err != nil {
		return runner.ChildHandoffCompletion{}, err
	}
	for {
		child, err := h.child(ctx, s, request)
		if err != nil {
			return runner.ChildHandoffCompletion{}, err
		}
		if child.State.Terminal() {
			url, err := h.repoCloneURL(h.project)
			if err != nil {
				return runner.ChildHandoffCompletion{}, err
			}
			coordinator := childworkflow.WorkspaceCoordinator{Queue: s.childQueue, Worktrees: h.worktrees}
			result, err := coordinator.ReadResult(ctx, child, url)
			if err != nil {
				return runner.ChildHandoffCompletion{}, err
			}
			return runner.ChildHandoffCompletion{State: string(result.Input.State), Summary: result.Input.Summary, ResultRef: result.ResultRef, WorkspaceRef: result.WorkspaceRef, References: result.Input.References}, nil
		}
		if err := childHandoffPoll(ctx, time.Second); err != nil {
			return runner.ChildHandoffCompletion{}, err
		}
	}
}

func (h *daemonChildHandoff) SuspendChildParent(ctx context.Context, runID string) (runner.ChildParentSuspension, error) {
	s, err := h.service()
	if err != nil {
		return nil, err
	}
	scheduler := s.childDispatch.sched.Load()
	if scheduler == nil {
		return nil, fmt.Errorf("child wait scheduler is unavailable")
	}
	suspension, err := scheduler.SuspendChildParent(ctx, runID)
	if err != nil {
		return nil, err
	}
	return daemonChildSuspension{suspension}, nil
}

type daemonChildSuspension struct {
	underlying *localscheduler.ChildParentSuspension
}

func (s daemonChildSuspension) Resume(ctx context.Context) error {
	err := s.underlying.Resume(ctx)
	var rejected *localscheduler.TriggerRejectedError
	if errors.As(err, &rejected) {
		return &runner.ChildCapacityWaitError{Reason: rejected.Reason, PolicyBlocked: rejected.Reason != localscheduler.ReasonMaxParallel && rejected.Reason != localscheduler.ReasonInstanceMaxParallel}
	}
	return err
}

func childHandoffPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func childRepoKey(ref apiv1.RepoRef) string {
	return (providers.RepositoryRef{Provider: providers.ProviderKind(ref.Provider), URL: ref.BaseURL, Owner: ref.Owner, Project: ref.Project, Name: ref.Name}).CanonicalKey()
}
