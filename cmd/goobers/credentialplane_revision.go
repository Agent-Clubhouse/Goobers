package main

import (
	"context"
	"errors"
	"net/http"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func (s *daemonCredentialService) resolveRevisionCheckout(ctx context.Context, request httpapi.CredentialResolveRequest, defs credentialPlaneDefinitions, reader *journal.Reader, identity journal.RunIdentity, machine *workflow.Machine) (httpapi.CredentialResolveResponse, error) {
	const checkoutKey = httpapi.WorkspaceRevisionCheckoutCapability
	refuse := func(status int, code, message string) (httpapi.CredentialResolveResponse, error) {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(status, code, message)
	}
	if len(request.Capabilities) > 1 || len(request.Capabilities) == 1 && request.Capabilities[0] != checkoutKey {
		return refuse(http.StatusForbidden, "capability_undeclared", "selected-revision checkout cannot resolve stage credentials")
	}
	readOnly := false
	if task, ok := machine.Task(request.Stage); ok {
		readOnly = task.EffectiveWorkspace() == apiv1.WorkspaceRepoReadOnly
	}
	if gate, ok := machine.Gate(request.Stage); ok {
		readOnly = gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil && gate.EffectiveWorkspace() == apiv1.WorkspaceRepoReadOnly
	}
	if !readOnly {
		return refuse(http.StatusForbidden, workspacerevision.CodeUnauthorized, "selected-revision checkout requires a pinned repo-readonly stage")
	}
	scope, ok := defs.Scopes[identity.Gaggle]
	if !ok {
		return refuse(http.StatusConflict, "gaggle_unavailable", "the run's gaggle is no longer configured")
	}
	events, err := reader.Events()
	if err != nil {
		return refuse(http.StatusInternalServerError, "run_read_failed", "run events could not be read")
	}
	selected, err := runner.RestoredWorkspaceRevision(events, machine, scope.Project, scope.AdditionalRepos)
	if err != nil || selected == nil {
		return refuse(http.StatusConflict, workspacerevision.CodeUnauthorized, "the run has no verifiable accepted workspace revision")
	}
	if _, err := workspacerevision.Accept(selected, request.WorkspaceRevision, true, true); err != nil {
		return refuse(http.StatusConflict, workspacerevision.CodeConflict, "checkout revision differs from the run's accepted revision")
	}
	source, err := workspacerevision.Resolve(*selected, scope.Project, scope.AdditionalRepos)
	if err != nil {
		return refuse(http.StatusForbidden, workspacerevision.CodeUnauthorized, "selected repository is not authorized for this run")
	}

	// Even the base repository uses its repository-qualified read grant.
	// Generic capability overrides and daemon write credentials are not fallback
	// sources for this checkout-only path.
	checkoutScope := credentialGaggleScope{Project: scope.Project, AdditionalRepos: []apiv1.RepoRef{source}}
	resolver, grants, err := s.credentialSources(checkoutScope)
	if err != nil {
		return refuse(http.StatusInternalServerError, "credential_wiring_failed", "checkout credential sources could not be constructed")
	}
	owner := source.Owner
	if source.Provider == apiv1.ProviderADO && source.Project != "" {
		owner += "/" + source.Project
	}
	key := credentials.RepoScopedCapability(string(capability.ContentsRead), owner, source.Name)
	injector, err := credentials.NewInjector(resolver, deterministicCredentialGrants(grants), s.shared)
	if err != nil {
		return refuse(http.StatusInternalServerError, "credential_wiring_failed", "checkout credential scope could not be constructed")
	}
	set, err := injector.Materialize(ctx, []string{key})
	if err != nil {
		return refuse(http.StatusBadGateway, "credential_resolution_failed", "the selected repository's read credential could not be resolved")
	}
	minted := make([]httpapi.MintedCredential, 0, 1)
	materialized := make([]string, 0, 1)
	token, err := set.Token(ctx, key)
	switch {
	case err == nil:
		entry := httpapi.MintedCredential{Capability: checkoutKey, Value: token}
		if expiry, ok := set.Expiry(key); ok {
			entry.ExpiresAt = &expiry
		}
		minted = append(minted, entry)
		materialized = append(materialized, key)
	case errors.Is(err, credentials.ErrNoCredentialForCapability):
		if checkoutAuthenticationConfigured(s.config, source) {
			return refuse(http.StatusForbidden, workspacerevision.CodeUnauthorized, "configured source authentication has no repository-qualified read grant")
		}
		// A configured public repository may intentionally have no credential.
		// A configured source that fails to resolve was refused above.
	default:
		return refuse(http.StatusBadGateway, "credential_resolution_failed", "the selected repository's read credential is unavailable")
	}
	if err := s.log.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, Gaggle: identity.Gaggle,
		Workflow: identity.Workflow, RunID: request.RunID, Stage: request.Stage,
		Runner: map[string]any{
			"kind": credentialResolutionMarker, "requested": []string{key}, "materialized": materialized,
		},
	}); err != nil {
		return refuse(http.StatusInternalServerError, "audit_failed", "checkout credential resolution could not be journaled")
	}
	return httpapi.CredentialResolveResponse{RunID: request.RunID, Stage: request.Stage, Credentials: minted}, nil
}

func checkoutAuthenticationConfigured(config *instance.Config, source apiv1.RepoRef) bool {
	if config == nil {
		return false
	}
	for _, repo := range config.Repos {
		if repo.Provider == string(source.Provider) && repo.Owner == source.Owner && repo.Project == source.Project && repo.Name == source.Name {
			return repo.Token.Configured() || repo.Auth != nil
		}
	}
	return false
}
