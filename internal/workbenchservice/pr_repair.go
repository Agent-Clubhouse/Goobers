package workbenchservice

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

// PRRepairClient exposes only bounded immutable reads and native expected-head CAS.
type PRRepairClient interface {
	providers.PRRepairReader
	providers.PRRepairWriter
}

// PRRepairFactory receives only an exact source and current interactive credential.
type PRRepairFactory func(context.Context, ReadBinding, interactiveaccess.Credential) (PRRepairClient, error)

// PRRepairCustody excludes live automation/managed worktree ownership while the
// synchronous callback joins provider work and receipt persistence. It must use
// the existing host claims lock, never a SQLite transaction across provider I/O.
type PRRepairCustody func(context.Context, providers.RepairPullRequest, func(context.Context) error) error

// PRRepairService is installed only with actual host repository custody checks.
// It cannot publish using an automation credential or an unselected repository.
type PRRepairService struct {
	Queue       *triggerqueue.Store
	Client      PRRepairFactory
	WithCustody PRRepairCustody
	Scrubber    journal.Scrubber
	Now         func() time.Time
}

// SessionPRRepair retains the actual selected message and current execution lease.
// All calls must join before that lease closes.
type SessionPRRepair struct {
	service   *PRRepairService
	retained  *apiv1.Gaggle
	lease     *interactiveaccess.ExecutionLease
	actor     sessioning.Actor
	identity  journal.RunIdentity
	origin    sessioning.PRRepairOrigin
	selection sessioning.PRRepairTarget
}

// ForSession verifies the actual published turn and immutable human selection.
func (s *PRRepairService) ForSession(ctx context.Context, retained *apiv1.Gaggle, lease *interactiveaccess.ExecutionLease, actor sessioning.Actor, identity journal.RunIdentity) (*SessionPRRepair, error) {
	if s == nil || s.Queue == nil || s.Client == nil || s.WithCustody == nil || s.Scrubber == nil || retained == nil || lease == nil {
		return nil, interactiveaccess.ErrDenied
	}
	if identity.Session != nil {
		lineage := *identity.Session
		identity.Session = &lineage
	}
	result := &SessionPRRepair{service: s, retained: retained.DeepCopy(), lease: lease, actor: actor, identity: identity}
	origin, selection, err := verifySessionOperationOrigin(ctx, s.Queue, identity, retained.Name, actor)
	if err != nil {
		return nil, err
	}
	if selection == nil {
		return nil, interactiveaccess.ErrDenied
	}
	result.origin, result.selection = origin, *selection
	if _, err = result.authorize(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Target returns a copy of the original immutable human selection.
func (s *SessionPRRepair) Target() sessioning.PRRepairTarget { return s.selection }

func (s *SessionPRRepair) authorize(ctx context.Context) (ReadBinding, error) {
	if err := ctx.Err(); err != nil {
		return ReadBinding{}, err
	}
	if err := s.lease.RequireSessionScope(s.retained.Name, s.actor.Issuer, s.actor.Subject); err != nil {
		return ReadBinding{}, err
	}
	if err := s.lease.RequireWorkbenchSources(s.retained); err != nil {
		return ReadBinding{}, err
	}
	bound, err := selectRepository(s.retained, s.selection.SourceBindingID)
	if err != nil {
		return ReadBinding{}, err
	}
	r := bound.Source.Spec.Repository
	selected := sessioning.RepairRepository{Provider: string(r.Provider), Owner: r.Owner, Project: r.Project, Name: r.Name}
	if selected != s.selection.Repository {
		return ReadBinding{}, interactiveaccess.ErrDenied
	}
	return bound, s.lease.AuthorizePRRepair(*r)
}

func (s *SessionPRRepair) use(ctx context.Context, use func(context.Context, ReadBinding) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.lease.Context(), cancel)
	defer stop()
	bound, err := s.authorize(ctx)
	if err != nil {
		return err
	}
	origin, selection, err := verifySessionOperationOrigin(ctx, s.service.Queue, s.identity, s.retained.Name, s.actor)
	if err != nil {
		return err
	}
	if origin != s.origin || selection == nil || *selection != s.selection {
		return interactiveaccess.ErrDenied
	}
	return use(ctx, bound)
}

func (s *SessionPRRepair) adapter(ctx context.Context, bound ReadBinding) (PRRepairClient, error) {
	credential, err := s.lease.Credential(ctx, "pr.repair", interactiveaccess.Target{Kind: "repository", Repository: *bound.Source.Spec.Repository})
	if err != nil {
		return nil, err
	}
	return s.service.Client(ctx, bound, credential)
}
func (s *SessionPRRepair) scope() triggerqueue.WorkbenchCommandScope {
	return triggerqueue.WorkbenchCommandScope{Gaggle: s.retained.Name, SourceBindingID: s.selection.SourceBindingID, Actor: s.actor}
}
func (s *PRRepairService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *PRRepairService) exact(value any) bool {
	raw, err := json.Marshal(value)
	return err == nil && bytes.Equal(raw, s.Scrubber.Scrub(raw))
}

// PRRepair reuses exact interactive credentials and scoped conditional reads;
// native mutation implementations do not perform automatic mutation retries.
func (f ProviderFactory) PRRepair(ctx context.Context, binding ReadBinding, credential interactiveaccess.Credential) (PRRepairClient, error) {
	if binding.Source.Spec.Repository == nil {
		return nil, interactiveaccess.ErrDenied
	}
	client, err := f.provider(ctx, binding, *binding.Source.Spec.Repository, credential)
	if err != nil {
		return nil, err
	}
	repair, ok := client.(PRRepairClient)
	if !ok {
		return nil, providers.ErrPRRepair
	}
	return repair, nil
}
