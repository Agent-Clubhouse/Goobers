package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
)

type childInteractiveCredentialKey struct{}
type childInteractiveCredentialScope struct {
	run     string
	lease   *interactiveaccess.ExecutionLease
	sources credentialGaggleScope
}

// Policy publication takes interactive authority before child authority. Begin
// this revocable operation before taking a child lease; Credential then uses the
// immutable lease policy without acquiring either lock again. A separate lease
// owned by the execution must span the full model worker's token lifetime.
func (s *daemonCredentialService) beginInteractiveChildCredentials(ctx context.Context, pinned pinnedStage) (context.Context, func(), error) {
	id := pinned.identity
	if id.Child == nil || id.Child.ExecutionEpoch == 0 {
		return ctx, func() {}, nil
	}
	if prior, ok := ctx.Value(childInteractiveCredentialKey{}).(*childInteractiveCredentialScope); ok {
		if prior.run != id.RunID || prior.lease.Context().Err() != nil {
			return nil, nil, interactiveChildCredentialRefusal()
		}
		return ctx, func() {}, nil
	}
	if s.interactive == nil || s.shared == nil || s.config == nil || id.ValidateChildLineage() != nil {
		return nil, nil, interactiveChildCredentialRefusal()
	}
	dir, err := s.layout.FindRunDir(id.RunID)
	if err != nil {
		return nil, nil, interactiveChildCredentialRefusal()
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return nil, nil, interactiveChildCredentialRefusal()
	}
	authority, err := interactiveaccess.LoadRestartAuthority(reader, id)
	if err != nil {
		return nil, nil, interactiveChildCredentialRefusal()
	}
	scope, ok := pinned.defs.Scopes[id.Gaggle]
	if !ok {
		return nil, nil, interactiveChildCredentialRefusal()
	}
	lease, err := s.interactive.BeginExecution(ctx, authority.Principal(), id.Gaggle)
	if err != nil {
		return nil, nil, interactiveChildCredentialRefusal()
	}
	if err = lease.RequireSources(scope.Project, scope.Backlog, scope.AdditionalRepos); err != nil {
		lease.Close()
		return nil, nil, interactiveChildCredentialRefusal()
	}
	bound := &childInteractiveCredentialScope{run: id.RunID, lease: lease, sources: scope}
	return context.WithValue(lease.Context(), childInteractiveCredentialKey{}, bound), lease.Close, nil
}

func interactiveChildCredentialRefusal() error {
	return credentialPlaneError(http.StatusForbidden, "child_interactive_credentials_unavailable", "The child restart has no verified current human credential authority.")
}

// This path is deliberately selected before automation injector construction.
// The model process still receives only its signed ModelOnly ceiling; provider
// material is available solely to the host's canonical publication operation.
func (s *daemonCredentialService) mintInteractiveChildCredentials(ctx context.Context, pinned pinnedStage, requested []string) (stageResolution, error) {
	bound, ok := ctx.Value(childInteractiveCredentialKey{}).(*childInteractiveCredentialScope)
	if !ok || bound.run != pinned.identity.RunID || bound.lease.Context().Err() != nil {
		return stageResolution{}, interactiveChildCredentialRefusal()
	}
	keys := append(slices.Clone(requested), credentials.FilterChildCredentialKeys(ctx, pinned.profile.implicitKeys)...)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	result := stageResolution{profile: pinned.profile, minted: []httpapi.MintedCredential{}}
	for _, key := range keys {
		value, credentialed, err := s.interactiveChildCredential(ctx, bound, pinned, key)
		if err != nil {
			return stageResolution{}, interactiveChildCredentialRefusal()
		}
		if !credentialed {
			continue
		}
		if value.Value == "" {
			return stageResolution{}, interactiveChildCredentialRefusal()
		}
		s.shared.Register([]byte(value.Value))
		entry := httpapi.MintedCredential{Capability: key, Value: value.Value}
		if !value.ExpiresAt.IsZero() {
			expiry := value.ExpiresAt
			entry.ExpiresAt = &expiry
		}
		if key != "agent:model" {
			if result.scheme != "" && result.scheme != value.Scheme {
				return stageResolution{}, interactiveChildCredentialRefusal()
			}
			result.scheme = value.Scheme
		}
		result.minted = append(result.minted, entry)
		result.materialized = append(result.materialized, key)
	}
	if ctx.Err() != nil || bound.lease.Context().Err() != nil {
		return stageResolution{}, interactiveChildCredentialRefusal()
	}
	return result, nil
}

func (s *daemonCredentialService) interactiveChildCredential(ctx context.Context, bound *childInteractiveCredentialScope, pinned pinnedStage, key string) (interactiveaccess.Credential, bool, error) {
	if key == "agent:model" {
		value, err := s.interactiveChildModelCredential(ctx, pinned)
		return value, true, err
	}
	action, kind, ok := interactiveCapability(key)
	if !ok {
		return interactiveaccess.Credential{}, false, interactiveaccess.ErrDenied
	}
	if kind == "" {
		return interactiveaccess.Credential{}, false, nil
	}
	target := interactiveaccess.Target{Kind: kind}
	if kind == "repository" {
		target.Repository = interactiveRepository(bound.sources.Project)
	}
	value, err := bound.lease.Credential(ctx, action, target)
	return value, true, err
}

func (s *daemonCredentialService) interactiveChildModelCredential(ctx context.Context, pinned pinnedStage) (interactiveaccess.Credential, error) {
	spec, ok := pinned.defs.Goobers[pinned.profile.goober]
	if !ok || (spec.Harness != apiv1.HarnessClaudeCode && spec.Harness != apiv1.HarnessCodex) || string(spec.Harness) != pinned.profile.harness || len(spec.MCPServers) != 0 || harness.CodexUsesAmbientChatGPT(spec.HarnessOptions) || len(s.config.Runner.HarnessCommand[string(spec.Harness)]) != 0 {
		return interactiveaccess.Credential{}, interactiveaccess.ErrCredentialUnavailable
	}
	grant, _ := agentModelGrant(s.config, spec.Harness)
	if grant == nil || grant.GitHubApp != nil || grant.Token.GitHubCLI != nil {
		return interactiveaccess.Credential{}, interactiveaccess.ErrCredentialUnavailable
	}
	resolve, _, err := agentModelCredentialExpiringResolver(s.config, s.stores, spec.Harness)
	if err != nil || resolve == nil {
		return interactiveaccess.Credential{}, interactiveaccess.ErrCredentialUnavailable
	}
	value, expiry, err := resolve(ctx)
	if err != nil || !validInteractiveModelAPIKey(spec.Harness, value) {
		return interactiveaccess.Credential{}, interactiveaccess.ErrCredentialUnavailable
	}
	return interactiveaccess.Credential{Value: value, Scheme: "bearer", ExpiresAt: expiry}, nil
}

func (p *childStagePod) beginPublicationCredentialContext(ctx context.Context, env apiv1.InvocationEnvelope) (context.Context, func(), error) {
	if p.identity.Child == nil || p.identity.Child.ExecutionEpoch == 0 {
		return ctx, func() {}, nil
	}
	stage, ok := strings.CutPrefix(env.TaskID, p.identity.RunID+":")
	if !ok {
		return nil, nil, errors.New("child publication stage identity differs")
	}
	pinned, err := p.service.loadPinnedStage(ctx, httpapi.CredentialResolveRequest{RunID: p.identity.RunID, Stage: stage})
	if err != nil {
		return nil, nil, err
	}
	bound, closeHuman, err := p.service.beginInteractiveChildCredentials(ctx, pinned)
	if err != nil {
		pinned.release()
		return nil, nil, err
	}
	return bound, func() { closeHuman(); pinned.release() }, nil
}
