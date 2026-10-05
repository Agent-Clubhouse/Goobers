package interactiveaccess

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

// PRRepairReviewAccess checks exact current source binding without minting a token.
type PRRepairReviewAccess func(Target) (RepositoryCredentialLoader, error)

// WithPRRepairReview fences operator/source authority through one bounded
// observation and its receipt save. Callbacks must not nest policy locks.
func (s *Service) WithPRRepairReview(ctx context.Context, p httpapi.Principal, gaggle string, use func(context.Context, *apiv1.Gaggle, PRRepairReviewAccess) error) error {
	if use == nil || !human(p) || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
		return ErrDenied
	}
	if err := s.lockSourceSnapshot(ctx); err != nil {
		return err
	}
	defer s.mu.RUnlock()
	g := s.gaggles[gaggle]
	for _, action := range []apiv1.InteractiveAction{"repository.read", "pr.repair"} {
		if err := authorize(p, g, action); err != nil {
			return err
		}
	}
	access := func(target Target) (RepositoryCredentialLoader, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if target.Kind != "repository" {
			return nil, ErrDenied
		}
		if _, err := selectSource(g, s.sources, target); err != nil {
			return nil, err
		}
		return func(call context.Context) (Credential, error) {
			if err := ctx.Err(); err != nil {
				return Credential{}, err
			}
			return s.restartCredential(call, p, g, "pr.repair", target)
		}, nil
	}
	if err := use(ctx, g.DeepCopy(), access); err != nil {
		return err
	}
	return ctx.Err()
}
