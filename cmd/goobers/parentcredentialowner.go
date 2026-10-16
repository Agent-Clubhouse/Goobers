package main

import (
	"context"
	"net/http"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

type parentCredentialPlane struct{ service *daemonCredentialService }

func (p parentCredentialPlane) Resolve(ctx context.Context, request httpapi.CredentialResolveRequest) (httpapi.CredentialResolveResponse, error) {
	if p.service == nil || request.Grant {
		return httpapi.CredentialResolveResponse{}, parentCredentialRefusal()
	}
	a, err := p.service.parentAttempt(ctx)
	if err != nil || request.RunID != a.contract.Identity.RunID || request.Stage != a.contract.Stage || (request.Attempt != 0 && int(request.Attempt) != a.contract.Attempt) || a.active(ctx) != nil || a.custody(ctx) != nil {
		return httpapi.CredentialResolveResponse{}, parentCredentialRefusal()
	}
	return p.service.Resolve(ctx, request)
}

func parentCredentialRefusal() error {
	return credentialPlaneError(http.StatusForbidden, "parent_attempt_unavailable", "parent credentials require the exact active execution contract and current policy")
}

// Child delegation limits do not define the parent's own model permission.
// This callback runs under Runtime's applied-policy lease; no mutable config
// files become current authority while an existing attempt resolves secrets.
func (s *daemonCredentialService) pinnedChildStage(ctx context.Context, current *instance.ConfigSet, parent journal.RunIdentity, stage string) (childworkflow.PinnedStageAdmission, error) {
	authority, err := loadPinnedChildStage(ctx, s.layout, s.config, current, parent, stage)
	if err != nil {
		return childworkflow.PinnedStageAdmission{}, err
	}
	policy, err := childCurrentCustodyPolicy(s.config, current, childworkflow.ParentSelection{Gaggle: parent.Gaggle, Workflow: parent.Workflow, Stage: stage})
	if err != nil {
		return childworkflow.PinnedStageAdmission{}, err
	}
	capabilities := childPermissionIntersection(authority.Admission.ParentTask.Capabilities, policy.GrantedCapabilities)
	if policy.ParentTask.Goober != authority.Admission.ParentTask.Goober {
		capabilities = nil
	}
	return childworkflow.PinnedStageAdmission{Admission: authority.Admission, ConfigGeneration: authority.ConfigGeneration, WorkflowDigest: authority.ParentWorkflowDigest, GooberDigest: authority.ParentGooberDigest, ParentExecutionCapabilities: capabilities}, nil
}

func (s *daemonCredentialService) applyParentCredentialCeiling(ctx context.Context, pinned pinnedStage, stage []string) (context.Context, *childCredentialLease, error) {
	a, err := s.parentAttempt(ctx)
	if err != nil || s.children == nil || len(stage) != 1 || stage[0] != a.contract.Stage || !reflect.DeepEqual(a.contract.Identity, pinned.identity) || a.active(ctx) != nil || a.custody(ctx) != nil {
		return nil, nil, parentCredentialRefusal()
	}
	current, release, err := s.children.AcquirePinnedStage(ctx, pinned.identity, stage[0])
	if err != nil {
		return nil, nil, parentCredentialRefusal()
	}
	ceiling := credentials.NewChildCeiling(false, current.Admission.ParentTask.Capabilities, current.ParentExecutionCapabilities)
	if current.ConfigGeneration != pinned.identity.ConfigGeneration || current.WorkflowDigest != pinned.identity.WorkflowDigest || current.GooberDigest != pinned.identity.GooberDigest || current.Admission.ParentTask.Type != apiv1.TaskAgentic || !sameChildCeiling(ceiling, a.contract.Ceiling) {
		release()
		return nil, nil, parentCredentialRefusal()
	}
	childCtx, err := credentials.WithChildCeiling(ctx, ceiling)
	if err != nil {
		release()
		return nil, nil, parentCredentialRefusal()
	}
	lease := &childCredentialLease{ceiling: ceiling, release: release, verify: func(ctx context.Context) error {
		if err := a.active(ctx); err != nil {
			return err
		}
		return a.custody(ctx)
	}}
	return childCtx, lease, nil
}
