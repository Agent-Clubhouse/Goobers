package childworkflow

import (
	"context"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// VerifyActiveStage checks durable execution ownership without granting new
// policy authority. It is suitable for bounded teardown artifact custody after
// a policy revocation; credential issuance must use AcquirePreparedStage.
func VerifyActiveStage(ctx context.Context, reader *journal.Reader, run string, origin apiv1.ChildWorkflowOrigin) (journal.RunIdentity, journal.Event, error) {
	return activeJournalStage(ctx, reader, run, origin)
}

// PreparedStageLease fences current child-delegation policy through a bounded
// host operation. Release is mandatory. Verify must run before delivering any
// secret or accepted effect; it refuses a concurrently ended/replaced attempt.
type PreparedStageLease struct {
	Authority Authority
	Release   func()
	Verify    func(context.Context) error
}

func (r *Runtime) AcquirePreparedStage(ctx context.Context, env apiv1.InvocationEnvelope) (PreparedStageLease, error) {
	r.mu.RLock()
	var once sync.Once
	release := func() { once.Do(r.mu.RUnlock) }
	authority, err := r.resolver.PrepareStage(ctx, env)
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
		return ctx.Err()
	}}, nil
}
