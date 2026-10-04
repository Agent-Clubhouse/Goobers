package interactiveaccess

import (
	"context"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

// SourceCredentialLoader resolves a host-selected source inside WithSourceRead.
// It cannot be retained or called after its bounded callback returns.
type SourceCredentialLoader func(context.Context, Target) (Credential, error)

// WithSourceRead binds source selection and credentials to the same applied
// configuration. The callback receives a private copy for structural validation;
// it must finish bounded provider reads before returning. It cannot nest another
// policy operation or retain the loader/credential for asynchronous execution.
func (s *Service) WithSourceRead(ctx context.Context, p httpapi.Principal, gaggle string, action apiv1.InteractiveAction, use func(context.Context, *apiv1.Gaggle, SourceCredentialLoader) error) error {
	if use == nil || (action != "backlog.read" && action != "repository.read") {
		return ErrDenied
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.gaggles[gaggle]
	if err := authorize(p, g, action); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return use(ctx, g.DeepCopy(), func(ctx context.Context, target Target) (Credential, error) {
		if !actionTarget(action, target) {
			return Credential{}, ErrDenied
		}
		return s.restartCredential(ctx, p, g, action, target)
	})
}

// WithSourceView permits bounded configuration metadata reads for an explicit
// gaggle viewer. It grants no credential or provider access. Gaggle is a copy.
func (s *Service) WithSourceView(ctx context.Context, p httpapi.Principal, gaggle string, use func(context.Context, *apiv1.Gaggle) error) error {
	if use == nil || !human(p) || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
		return ErrDenied
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	g := s.gaggles[gaggle]
	if g == nil {
		return ErrDenied
	}
	view, _ := membership(p, g.Spec.InteractiveAccess)
	if !view {
		return ErrDenied
	}
	return use(ctx, g.DeepCopy())
}

// RequireWorkbenchSources prevents a pinned session from resolving a newly
// configured source declaration. It uses the live lease without reentering the
// service policy lock, so policy revocation can cancel and join its execution.
func (l *ExecutionLease) RequireWorkbenchSources(gaggle *apiv1.Gaggle) error {
	if gaggle == nil || gaggle.Name != l.gaggle.Name {
		return ErrDenied
	}
	if err := l.RequireSources(gaggle.Spec.Project, gaggle.Spec.Backlog, gaggle.Spec.AdditionalRepos); err != nil {
		return err
	}
	if !reflect.DeepEqual(l.gaggle.Spec.Workbench, gaggle.Spec.Workbench) {
		return ErrDenied
	}
	return nil
}

// AuthorizeSourceRead checks the current execution lease without minting a
// credential or acquiring the service policy lock. It is an availability check;
// each actual read still resolves its exact target through Credential.
func (l *ExecutionLease) AuthorizeSourceRead(action apiv1.InteractiveAction) error {
	if action != "backlog.read" && action != "repository.read" {
		return ErrDenied
	}
	if err := l.ctx.Err(); err != nil {
		return err
	}
	return authorize(l.principal, l.gaggle, action)
}
