package interactiveaccess

import (
	"context"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

// SnapshotCredentialLoader authorizes one exact read target inside a source
// snapshot callback. It must not be retained or used asynchronously after return.
type SnapshotCredentialLoader func(context.Context, apiv1.InteractiveAction, Target) (Credential, error)

// WithSourceSnapshot selects one applied gaggle and its explicit human source
// visibility for a bounded aggregate read. Every provider source independently
// requires a read action and configured credential through load. The callback
// must finish all reads before returning and cannot nest policy operations.
func (s *Service) WithSourceSnapshot(ctx context.Context, p httpapi.Principal, gaggle string, use func(context.Context, *apiv1.Gaggle, SnapshotCredentialLoader) error) error {
	if use == nil || !human(p) || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
		return ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	if err := s.lockSourceSnapshot(ctx); err != nil {
		return err
	}
	defer s.mu.RUnlock()
	g := s.gaggles[gaggle]
	if g == nil {
		return ErrDenied
	}
	view, _ := membership(p, g.Spec.InteractiveAccess)
	if !view {
		return ErrDenied
	}
	load := func(readctx context.Context, action apiv1.InteractiveAction, target Target) (Credential, error) {
		if err := ctx.Err(); err != nil {
			return Credential{}, err
		}
		if !readAction(action) || !actionTarget(action, target) {
			return Credential{}, ErrDenied
		}
		if err := authorize(p, g, action); err != nil {
			return Credential{}, err
		}
		return s.restartCredential(readctx, p, g, action, target)
	}
	if err := use(ctx, g.DeepCopy(), load); err != nil {
		return err
	}
	return ctx.Err()
}

// A policy writer may be joining executions. Waiting for its lock is included
// in the aggregate deadline; a cancelled request must not wait for that drain.
func (s *Service) lockSourceSnapshot(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.mu.TryRLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
