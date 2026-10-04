package interactiveaccess

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

// WithSourceWrite fences one bounded native backlog command against current
// human policy. Selection is checked without minting a token; duplicate/receipt
// inspection can return without calling load. Only a newly claimed effect may
// resolve its credential. The callback must join provider work and persist its
// receipt before returning; it must not retain load or start asynchronous effects.
func (s *Service) WithSourceWrite(ctx context.Context, p httpapi.Principal, gaggle string, use func(context.Context, *apiv1.Gaggle, SourceCredentialLoader) error) error {
	if use == nil || !human(p) || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
		return ErrDenied
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.gaggles[gaggle]
	if err := authorize(p, g, "backlog.edit"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := selectSource(g, s.sources, Target{Kind: "backlog"}); err != nil {
		return err
	}
	return use(ctx, g.DeepCopy(), func(ctx context.Context, target Target) (Credential, error) {
		if target.Kind != "backlog" || target.Repository != (apiv1.InteractiveRepositoryIdentity{}) {
			return Credential{}, ErrDenied
		}
		return s.restartCredential(ctx, p, g, "backlog.edit", target)
	})
}
