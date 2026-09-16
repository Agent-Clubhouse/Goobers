package main

import (
	"context"
	"net/http"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func restoreCredentialBranch(defs credentialPlaneDefinitions, reader *journal.Reader, identity journal.RunIdentity, machine *workflow.Machine) (*apiv1.WorkspaceBranchBinding, error) {
	scope, ok := defs.Scopes[identity.Gaggle]
	if !ok {
		return nil, credentialPlaneError(http.StatusConflict, "gaggle_unavailable", "run gaggle is no longer configured")
	}
	events, err := reader.Events()
	if err != nil {
		return nil, credentialPlaneError(http.StatusInternalServerError, "run_read_failed", "run events could not be read")
	}
	binding, err := runner.RestoredWorkspaceBranchBinding(events, machine, scope.Project, scope.AdditionalRepos, scope.BranchNamespace, identity.RunID)
	if err != nil {
		return nil, credentialPlaneError(http.StatusConflict, workspacerevision.CodeConflict, "run branch ownership could not be verified")
	}
	return binding, nil
}

func (s *daemonCredentialService) resolveOwnedBranchCheckout(ctx context.Context, request httpapi.CredentialResolveRequest, defs credentialPlaneDefinitions, reader *journal.Reader, identity journal.RunIdentity, machine *workflow.Machine) (httpapi.CredentialResolveResponse, error) {
	binding, err := restoreCredentialBranch(defs, reader, identity, machine)
	if err != nil {
		return httpapi.CredentialResolveResponse{}, err
	}
	writable := false
	if task, ok := machine.Task(request.Stage); ok {
		mode := task.EffectiveWorkspace()
		writable = mode == "" || mode.IsWritableRepo()
	}
	if gate, ok := machine.Gate(request.Stage); ok {
		mode := gate.EffectiveWorkspace()
		writable = gate.Evaluator == apiv1.EvaluatorAgentic && (mode == "" || mode.IsWritableRepo())
	}
	if !writable || binding == nil || !reflect.DeepEqual(binding, request.WorkspaceBranchBinding) || len(request.Capabilities) != 0 {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(http.StatusForbidden, workspacerevision.CodeUnauthorized, "checkout requires the run's durable ownership and a pinned writable stage")
	}
	scope := defs.Scopes[identity.Gaggle]
	return s.resolveWorkspaceCheckoutGrant(ctx, request, scope, scope.Project, identity, httpapi.WorkspaceBranchCheckoutCapability)
}

// Every stage identity in an owned run is restricted, including attempts to
// impersonate the establishment/publish stage using the run-scoped pod token.
func (s *daemonCredentialService) restrictOwnedBranchCredentials(defs credentialPlaneDefinitions, reader *journal.Reader, identity journal.RunIdentity, machine *workflow.Machine, profile *stageProfile, request *httpapi.CredentialResolveRequest) error {
	binding, err := restoreCredentialBranch(defs, reader, identity, machine)
	if err != nil || binding == nil {
		return err
	}
	requested := request.Capabilities
	if len(requested) == 0 {
		requested = profile.capabilities
	}
	keys, err := workspacebranch.StageCredentialKeys(requested, true)
	if err != nil {
		return credentialPlaneError(http.StatusForbidden, workspacerevision.CodeUnauthorized, err.Error())
	}
	declared, err := workspacebranch.StageCredentialKeys(profile.capabilities, true)
	if err != nil {
		return credentialPlaneError(http.StatusForbidden, workspacerevision.CodeUnauthorized, err.Error())
	}
	profile.capabilities = declared
	profile.implicitKeys = nil
	request.Capabilities = keys
	return nil
}
