package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type parentAccessPlane struct{ service *daemonCredentialService }

func (a parentAttemptCustody) envelope(ctx context.Context) (apiv1.InvocationEnvelope, error) {
	if err := a.active(ctx); err != nil {
		return apiv1.InvocationEnvelope{}, err
	}
	id := a.contract.Identity
	machine, err := runner.PinnedWorkflowMachine(a.reader, id)
	if err != nil {
		return apiv1.InvocationEnvelope{}, err
	}
	task, found := machine.Task(a.contract.Stage)
	if !found || task.ChildWorkflows == nil || task.Type != apiv1.TaskAgentic {
		return apiv1.InvocationEnvelope{}, childworkflow.ErrAuthorityUnavailable
	}
	return apiv1.InvocationEnvelope{RunID: id.RunID, TaskID: strings.Join([]string{id.RunID, a.contract.Stage}, ":"), InstanceID: id.InstanceID, Gaggle: id.Gaggle, WorkflowID: id.Workflow, Goober: task.Goober, GooberDigest: id.GooberDigest, ConfigGeneration: id.ConfigGeneration, Attempt: int32(a.contract.Attempt), Capabilities: task.Capabilities, ChildWorkflowOrigin: a.contract.ParentOrigin}, nil
}

func (p parentAccessPlane) AcquireChildWorkflowAccess(ctx context.Context, run, digest string) (httpapi.ChildWorkflowAccessResponse, error) {
	s := p.service
	if s == nil {
		return httpapi.ChildWorkflowAccessResponse{}, parentCredentialRefusal()
	}
	a, err := s.parentAttempt(ctx)
	if err != nil {
		return httpapi.ChildWorkflowAccessResponse{}, errors.Join(parentCredentialRefusal(), err)
	}
	if a.contract.Identity.RunID != run || a.digest != digest || s.children == nil || s.shared == nil || a.custody(ctx) != nil {
		return httpapi.ChildWorkflowAccessResponse{}, parentCredentialRefusal()
	}
	env, err := a.envelope(ctx)
	if err != nil {
		return httpapi.ChildWorkflowAccessResponse{}, errors.Join(parentCredentialRefusal(), err)
	}
	access, revoke, err := s.children.AcquireForContainedPod(ctx, env, s.shared)
	if err != nil {
		return httpapi.ChildWorkflowAccessResponse{}, errors.Join(parentCredentialRefusal(), err)
	}
	if err = errors.Join(ctx.Err(), a.active(ctx), a.custody(ctx)); err != nil {
		return httpapi.ChildWorkflowAccessResponse{}, errors.Join(parentCredentialRefusal(), err, revoke())
	}
	return httpapi.ChildWorkflowAccessResponse{Endpoint: access.Endpoint, BearerToken: access.BearerToken}, nil
}

func (p parentAccessPlane) RevokeChildWorkflowAccess(ctx context.Context, run, digest string) error {
	s := p.service
	if s == nil {
		return parentCredentialRefusal()
	}
	a, err := s.parentAttempt(ctx)
	if err != nil {
		return err
	}
	if a.contract.Identity.RunID != run || a.digest != digest || s.childQueue == nil {
		return parentCredentialRefusal()
	}
	origin := a.contract.ParentOrigin
	binding, err := s.childQueue.ChildAuthority(ctx, triggerqueue.ChildParent{Gaggle: a.contract.Identity.Gaggle, ParentRunID: run}, origin.StageOccurrence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Revocation is allowed after stage completion, but an old token must never
	// revoke the new attempt's grant. No policy expansion follows from revocation.
	if binding.AttemptID != origin.AttemptID {
		return nil
	}
	if err = s.childQueue.RevokeChildAuthority(ctx, binding); errors.Is(err, triggerqueue.ErrChildAuthorityChanged) {
		return nil
	}
	return err
}
