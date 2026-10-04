package workbenchservice

import (
	"context"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/providers"
)

// LearnedBlock is the exact target's host record, selected while claims.lock is
// held. Missing records have a target-bound digest and Complete=true; ambiguous
// legacy records have Complete=false and can never authorize marker removal.
type LearnedBlock struct {
	Digest, Reason string
	Blockers       []string
	Complete       bool
}

// LearnedBlockScope holds the existing claims lock through the synchronous
// callback, including joined provider work and receipt persistence. It neither
// changes the block record nor holds a SQLite transaction across use.
type LearnedBlockScope func(context.Context, providers.RepositoryRef, string, func(context.Context, LearnedBlock) error) error

// ResolutionService provides a distinct backlog.resolve operation. The host
// supplies current source readers, shared durable custody and claims ownership.
type ResolutionService struct {
	ReadService      *Service
	Queue            *triggerqueue.Store
	WithLearnedBlock LearnedBlockScope
	Now              func() time.Time
}

// SessionResolver preserves exact current human/session/source authority. The
// host must join its calls before closing the already-open execution lease.
type SessionResolver struct {
	service  *ResolutionService
	retained *apiv1.Gaggle
	lease    *interactiveaccess.ExecutionLease
	actor    sessioning.Actor
	identity journal.RunIdentity
	origin   workbench.NeedsHumanResolutionOrigin
}

// ForSession accepts only the actual runner's verified session identity, then
// checks it against immutable queue inputs and the currently executing turn.
func (s *ResolutionService) ForSession(ctx context.Context, retained *apiv1.Gaggle, lease *interactiveaccess.ExecutionLease, actor sessioning.Actor, identity journal.RunIdentity) (*SessionResolver, error) {
	if s == nil || s.ReadService == nil || s.ReadService.Backlog == nil || s.Queue == nil || s.WithLearnedBlock == nil || retained == nil || lease == nil {
		return nil, interactiveaccess.ErrDenied
	}
	if identity.Session != nil {
		lineage := *identity.Session
		identity.Session = &lineage
	}
	result := &SessionResolver{service: s, retained: retained.DeepCopy(), lease: lease, actor: actor, identity: identity}
	if err := result.authorize(ctx); err != nil {
		return nil, err
	}
	origin, err := result.verifyOrigin(ctx)
	if err != nil {
		return nil, err
	}
	result.origin = origin
	return result, nil
}
func (s *SessionResolver) authorize(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.lease.RequireSessionScope(s.retained.Name, s.actor.Issuer, s.actor.Subject); err != nil {
		return err
	}
	if err := s.lease.RequireWorkbenchSources(s.retained); err != nil {
		return err
	}
	return s.lease.AuthorizeSourceResolve()
}
func (s *SessionResolver) use(ctx context.Context, binding string, use func(context.Context, ReadBinding) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.lease.Context(), cancel)
	defer stop()
	if err := s.authorize(ctx); err != nil {
		return err
	}
	if _, err := s.verifyOrigin(ctx); err != nil {
		return err
	}
	bound, err := selectBacklog(s.retained, binding)
	if err != nil {
		return err
	}
	if !workbenchprovider.NeedsHumanResolutionAllowed(bound.Source) {
		return interactiveaccess.ErrDenied
	}
	return use(ctx, bound)
}
func (s *SessionResolver) scope(binding string) triggerqueue.WorkbenchCommandScope {
	return triggerqueue.WorkbenchCommandScope{Gaggle: s.retained.Name, SourceBindingID: binding, Actor: s.actor}
}
func (s *ResolutionService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func resolutionRepository(bound ReadBinding) providers.RepositoryRef {
	id := bound.Source.BacklogIdentity
	return providers.RepositoryRef{Provider: providers.ProviderKind(id.Provider), Owner: id.Owner, Project: id.Project, Name: id.Name}
}
func (s *SessionResolver) adapter(ctx context.Context, bound ReadBinding) (*workbenchprovider.AttentionResolver, error) {
	credential, err := s.lease.Credential(ctx, "backlog.resolve", interactiveaccess.Target{Kind: "backlog"})
	if err != nil {
		return nil, err
	}
	client, err := s.service.ReadService.Backlog(ctx, bound, credential)
	if err != nil {
		return nil, err
	}
	native, ok := client.(workbenchprovider.AttentionClient)
	if !ok {
		return nil, workbenchprovider.ErrUnsupportedEdit
	}
	return workbenchprovider.NewAttentionResolver(bound.Scope, bound.Source, native)
}
