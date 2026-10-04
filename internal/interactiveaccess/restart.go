package interactiveaccess

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

// RestartSourceRequest selects only the sources needed to revalidate a restart.
// Nil Repository supports a scratch run; Backlog must be true when work-item
// claims are being reacquired. These are trusted host selections, not wire DTOs.
type RestartSourceRequest struct {
	Repository *apiv1.InteractiveRepositoryIdentity
	Backlog    bool
}

// RestartSources is scoped to WithRestartSources' bounded callback. It must not
// be retained by asynchronous stage executors; those require their own live
// interactive credential binding. Gaggle is a private copy of the current policy.
type RestartSources struct {
	Gaggle          *apiv1.Gaggle
	Repository      Credential
	Backlog         Credential
	BacklogIdentity apiv1.InteractiveRepositoryIdentity
}

// WithRestartSources revalidates permission and resolves independently selected
// repository/backlog identities under one policy lease. This avoids nesting
// WithAuthorization and WithCredential read locks, which can deadlock when a
// policy writer is waiting. The callback may verify source state and durably
// accept a restart; it must not execute or wait for the full run.
func (s *Service) WithRestartSources(ctx context.Context, p httpapi.Principal, gaggle string, request RestartSourceRequest, use func(context.Context, RestartSources) error) error {
	if use == nil {
		return ErrDenied
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.gaggles[gaggle]
	if err := authorize(p, g, "run.restartStage"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result := RestartSources{Gaggle: g.DeepCopy()}
	if request.Repository != nil {
		credential, err := s.restartCredential(ctx, p, g, "repository.read", Target{Kind: "repository", Repository: *request.Repository})
		if err != nil {
			return err
		}
		result.Repository = credential
	}
	if request.Backlog {
		credential, err := s.restartCredential(ctx, p, g, "backlog.read", Target{Kind: "backlog"})
		if err != nil {
			return err
		}
		result.Backlog = credential
		result.BacklogIdentity, _ = backlogIdentity(g)
	}
	return use(ctx, result)
}

func (s *Service) restartCredential(ctx context.Context, p httpapi.Principal, g *apiv1.Gaggle, action apiv1.InteractiveAction, target Target) (Credential, error) {
	if err := authorize(p, g, action); err != nil {
		return Credential{}, err
	}
	source, err := selectSource(g, s.sources, target)
	if err != nil {
		return Credential{}, err
	}
	return s.resolveCredential(ctx, source, action)
}
