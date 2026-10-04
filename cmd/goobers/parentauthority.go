package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type parentAttemptCustody struct {
	contract childpod.Contract
	digest   string
	blobs    childpod.ParentAttemptBlobs
	reader   *journal.Reader
}

// The signed physical-attempt digest is the only selector. Supplied identities,
// actor strings and a guessed run-owned digest confer no authority.
func (s *daemonCredentialService) parentAttempt(ctx context.Context) (parentAttemptCustody, error) {
	p, ok := httpapi.PrincipalFromContext(ctx)
	if !ok || !httpapi.IsPodPrincipal(p) || !p.WorkflowParent || p.GeneratedChild || !blobstore.ValidDigest(p.WorkflowParentContractDigest) {
		return parentAttemptCustody{}, childworkflow.ErrAuthorityUnavailable
	}
	run, valid := strings.CutPrefix(p.Subject, "run:")
	if !valid || !apiv1.ValidRunID(run) {
		return parentAttemptCustody{}, childworkflow.ErrAuthorityUnavailable
	}
	dir, err := s.layout.FindRunDir(run)
	if err != nil {
		return parentAttemptCustody{}, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return parentAttemptCustody{}, err
	}
	id, err := reader.Identity()
	if err != nil {
		return parentAttemptCustody{}, err
	}
	store := childpod.ParentBlobs{RunDir: dir, Identity: id}
	scoped := childpod.ParentAttemptBlobs{Store: store, ContractDigest: p.WorkflowParentContractDigest}
	raw, err := scoped.Get(ctx, p.WorkflowParentContractDigest)
	if err != nil {
		return parentAttemptCustody{}, err
	}
	contract, err := childpod.DecodeContract(raw, p.WorkflowParentContractDigest)
	if err != nil || contract.ParentOrigin == nil || !reflect.DeepEqual(contract.Identity, id) {
		return parentAttemptCustody{}, errors.New("parent contract differs from retained journal")
	}
	return parentAttemptCustody{contract: contract, digest: p.WorkflowParentContractDigest, blobs: scoped, reader: reader}, nil
}

func (a parentAttemptCustody) active(ctx context.Context) error {
	id, event, err := childworkflow.VerifyActiveStage(ctx, a.reader, a.contract.Identity.RunID, *a.contract.ParentOrigin)
	if err != nil {
		return err
	}
	if id.RunID != a.contract.Identity.RunID || event.Stage != a.contract.Stage || event.Attempt != a.contract.Attempt {
		return childworkflow.ErrAuthorityChanged
	}
	return nil
}

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
	return apiv1.InvocationEnvelope{RunID: id.RunID, TaskID: id.RunID + ":" + a.contract.Stage, InstanceID: id.InstanceID, Gaggle: id.Gaggle, WorkflowID: id.Workflow, Goober: task.Goober, GooberDigest: id.GooberDigest, ConfigGeneration: id.ConfigGeneration, Attempt: int32(a.contract.Attempt), Capabilities: task.Capabilities, ChildWorkflowOrigin: a.contract.ParentOrigin}, nil
}

func (s *daemonCredentialService) AcquireChildWorkflowAccess(ctx context.Context, run, digest string) (httpapi.ChildWorkflowAccessResponse, error) {
	a, err := s.parentAttempt(ctx)
	if err != nil {
		return httpapi.ChildWorkflowAccessResponse{}, err
	}
	if a.contract.Identity.RunID != run || a.digest != digest || s.children == nil || s.shared == nil {
		return httpapi.ChildWorkflowAccessResponse{}, childworkflow.ErrAuthorityUnavailable
	}
	env, err := a.envelope(ctx)
	if err != nil {
		return httpapi.ChildWorkflowAccessResponse{}, err
	}
	access, revoke, err := s.children.Acquire(ctx, env, s.shared)
	if err != nil {
		return httpapi.ChildWorkflowAccessResponse{}, err
	}
	if err = ctx.Err(); err != nil {
		return httpapi.ChildWorkflowAccessResponse{}, errors.Join(err, revoke())
	}
	return httpapi.ChildWorkflowAccessResponse{Endpoint: access.Endpoint, BearerToken: access.BearerToken}, nil
}

func (s *daemonCredentialService) RevokeChildWorkflowAccess(ctx context.Context, run, digest string) error {
	a, err := s.parentAttempt(ctx)
	if err != nil {
		return err
	}
	if a.contract.Identity.RunID != run || a.digest != digest || s.childQueue == nil {
		return childworkflow.ErrAuthorityUnavailable
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

func parentAuthorityRefusal() error {
	return credentialPlaneError(http.StatusForbidden, "parent_attempt_unavailable", "contained parent attempt authority is unavailable")
}
