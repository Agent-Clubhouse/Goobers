package interactiveaccess

import (
	"context"

	"github.com/goobers/goobers/internal/httpapi"
)

// WithQueueCancellation fences bounded durable acceptance against current human
// policy. It never grants a scoped machine token the human's queue authority.
func (s *Service) WithQueueCancellation(ctx context.Context, p httpapi.Principal, gaggle string, use func(context.Context) error) error {
	if use == nil || !human(p) || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
		return ErrDenied
	}
	if err := s.lockSourceSnapshot(ctx); err != nil {
		return err
	}
	defer s.mu.RUnlock()
	if err := authorize(p, s.gaggles[gaggle], "queue.cancel"); err != nil {
		return err
	}
	return use(ctx)
}

// SetQueueCancellationAvailable advertises the installed adapter, never a grant.
func (s *Service) SetQueueCancellationAvailable(available bool) {
	s.queueCancelAvailable.Store(available)
}
