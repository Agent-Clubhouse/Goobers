package interactiveaccess

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

// RepositoryCredentialLoader belongs to a single bounded repository callback.
// It cannot be retained or used after the callback returns.
type RepositoryCredentialLoader func(context.Context) (Credential, error)

// WithRepositorySource selects one exact configured repository under the same
// current-policy lease as the provider work. Selection validates the configured
// credential binding without minting a token, so receipt replay needs no token.
// Source changes always require PRs, including when sourceWrites is omitted.
// Callbacks must join provider work and persist effects before returning.
func (s *Service) WithRepositorySource(ctx context.Context, p httpapi.Principal, gaggle string, action apiv1.InteractiveAction, selectTarget func(*apiv1.Gaggle) (Target, error), use func(context.Context, RepositoryCredentialLoader) error) error {
	if selectTarget == nil || use == nil || !human(p) || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
		return ErrDenied
	}
	if action != "repository.read" && action != "source.proposeChange" {
		return ErrDenied
	}
	if err := s.lockSourceSnapshot(ctx); err != nil {
		return err
	}
	defer s.mu.RUnlock()
	g := s.gaggles[gaggle]
	if err := authorize(p, g, action); err != nil {
		return err
	}
	if g.Spec.InteractiveAccess.SourceWrites != nil && g.Spec.InteractiveAccess.SourceWrites.Mode != "pull-request" {
		return ErrDenied
	}
	target, err := selectTarget(g.DeepCopy())
	if err != nil {
		return err
	}
	if target.Kind != "repository" {
		return ErrDenied
	}
	if _, err = selectSource(g, s.sources, target); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return use(ctx, func(callctx context.Context) (Credential, error) {
		if err := ctx.Err(); err != nil {
			return Credential{}, err
		}
		return s.restartCredential(callctx, p, g, action, target)
	})
}
