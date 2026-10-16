package childworkflow

import (
	"context"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// PreparedStageLease fences current child-delegation policy through a bounded
// host operation. Release is mandatory. Verify must run before delivering any
// secret or accepted effect; it refuses a concurrently ended/replaced attempt.
type PreparedStageLease struct {
	Authority Authority
	Release   func()
	Verify    func(context.Context) error
}

// AcquirePreparedStage holds applied policy while checking live stage and parent cancellation.
func (r *Runtime) AcquirePreparedStage(ctx context.Context, env apiv1.InvocationEnvelope) (PreparedStageLease, error) {
	if r == nil {
		return PreparedStageLease{}, ErrAuthorityUnavailable
	}
	r.mu.RLock()
	var once sync.Once
	release := func() { once.Do(r.mu.RUnlock) }
	authority, err := r.resolver.PrepareStage(ctx, env)
	parent := triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}
	if err == nil {
		err = r.queue.CheckChildParentOpen(ctx, parent)
	}
	if err != nil {
		release()
		return PreparedStageLease{}, err
	}
	return PreparedStageLease{Authority: authority, Release: release, Verify: func(ctx context.Context) error {
		current, err := r.resolver.PrepareStage(ctx, env)
		if err != nil {
			return err
		}
		if !samePreparedAuthority(authority, current) {
			return ErrAuthorityChanged
		}
		if err := r.queue.CheckChildParentOpen(ctx, parent); err != nil {
			return err
		}
		return ctx.Err()
	}}, nil
}
