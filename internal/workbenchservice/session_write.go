package workbenchservice

import (
	"context"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

// SessionWriter shares durable command custody with manual edits. Its authority
// comes from the already-open, actor-bound execution lease, never a nested policy
// callback. The host must join all calls before closing that lease.
type SessionWriter struct {
	service  *WriterService
	retained *apiv1.Gaggle
	lease    *interactiveaccess.ExecutionLease
	actor    sessioning.Actor
}

// ForSession fixes the exact human and retained source topology for this turn.
// Permission is checked again before every acceptance, replay and receipt read.
func (s *WriterService) ForSession(retained *apiv1.Gaggle, lease *interactiveaccess.ExecutionLease, actor sessioning.Actor) (*SessionWriter, error) {
	if s == nil || s.ReadService == nil || s.ReadService.Backlog == nil || s.Queue == nil || retained == nil || lease == nil {
		return nil, interactiveaccess.ErrDenied
	}
	writer := &SessionWriter{service: s, retained: retained.DeepCopy(), lease: lease, actor: actor}
	if err := writer.authorize(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (s *SessionWriter) authorize() error {
	if err := s.lease.RequireSessionScope(s.retained.Name, s.actor.Issuer, s.actor.Subject); err != nil {
		return err
	}
	if err := s.lease.RequireWorkbenchSources(s.retained); err != nil {
		return err
	}
	return s.lease.AuthorizeSourceWrite("backlog.edit")
}

// Capabilities returns current retained-source native write fields.
func (s *SessionWriter) Capabilities(ctx context.Context, binding string) (workbench.BacklogWriteCapabilities, error) {
	var result workbench.BacklogWriteCapabilities
	err := s.use(ctx, binding, func(_ context.Context, bound ReadBinding, _ interactiveaccess.SourceCredentialLoader) error {
		result = workbenchprovider.BacklogCapabilities(bound.Source)
		return nil
	})
	return result, writeError(err)
}

// Patch accepts the host's turn-scoped command key. An exact duplicate reuses
// retained custody; a previous unknown effect is never sent to the provider again.
func (s *SessionWriter) Patch(ctx context.Context, binding, key string, request workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error) {
	request = copyWriteRequest(request)
	var result workbench.BacklogEditCommand
	err := s.use(ctx, binding, func(ctx context.Context, bound ReadBinding, load interactiveaccess.SourceCredentialLoader) error {
		var err error
		result, err = s.service.patch(ctx, s.scope(binding), bound, load, key, request)
		return err
	})
	return result, writeError(err)
}

// Command reads an exact actor/source receipt without stale revision preflight.
func (s *SessionWriter) Command(ctx context.Context, binding, id string) (workbench.BacklogEditCommand, error) {
	var result workbench.BacklogEditCommand
	err := s.use(ctx, binding, func(ctx context.Context, bound ReadBinding, _ interactiveaccess.SourceCredentialLoader) error {
		var err error
		result, err = s.service.command(ctx, s.scope(binding), bound, id)
		return err
	})
	return result, writeError(err)
}
func (s *SessionWriter) scope(binding string) triggerqueue.WorkbenchCommandScope {
	return triggerqueue.WorkbenchCommandScope{Gaggle: s.retained.Name, SourceBindingID: binding, Actor: s.actor}
}
func (s *SessionWriter) use(ctx context.Context, binding string, use func(context.Context, ReadBinding, interactiveaccess.SourceCredentialLoader) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.lease.Context(), cancel)
	defer stop()
	if err := s.authorize(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bound, err := selectBacklog(s.retained, binding)
	if err != nil {
		return err
	}
	return use(ctx, bound, func(ctx context.Context, target interactiveaccess.Target) (interactiveaccess.Credential, error) {
		if target.Kind != "backlog" || target.Repository != (apiv1.InteractiveRepositoryIdentity{}) {
			return interactiveaccess.Credential{}, interactiveaccess.ErrDenied
		}
		return s.lease.Credential(ctx, "backlog.edit", target)
	})
}
