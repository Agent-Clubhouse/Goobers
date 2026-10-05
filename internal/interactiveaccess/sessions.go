package interactiveaccess

import (
	"context"

	"github.com/goobers/goobers/internal/httpapi"
)

// WithSessionView authorizes shared conversation visibility independently of
// permission to create or send. Explicit gaggle membership remains required.
// The callback is bounded storage access and must not execute external work.
func (s *Service) WithSessionView(ctx context.Context, p httpapi.Principal, gaggle string, use func(context.Context) error) error {
	if use == nil || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
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
	return use(ctx)
}

// BeginSessionExecution shares the same cancellation/join and exact credential
// source machinery as a human restart, with the session message action checked
// at this asynchronous admission boundary.
func (s *Service) BeginSessionExecution(ctx context.Context, p httpapi.Principal, gaggle string) (*ExecutionLease, error) {
	return s.beginExecution(ctx, p, gaggle, "session.message")
}

// SetSessionsAvailable advertises an installed host runtime, never permission.
func (s *Service) SetSessionsAvailable(available bool) { s.sessionsAvailable.Store(available) }

// RequireSessionScope binds host tool authority and audit attribution to the
// exact human and gaggle that own this still-live session execution lease.
func (l *ExecutionLease) RequireSessionScope(gaggle, issuer, subject string) error {
	if l == nil || l.ctx.Err() != nil || l.gaggle.Name != gaggle || l.principal.Issuer != issuer || l.principal.Subject != subject {
		return ErrDenied
	}
	return authorize(l.principal, l.gaggle, "session.message")
}
