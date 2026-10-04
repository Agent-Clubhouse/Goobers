package workbenchservice

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

// SessionReader uses an already-live human lease. It never reacquires the policy
// lock, including while policy Apply is cancelling and joining that execution.
type SessionReader struct {
	service  *Service
	retained *apiv1.Gaggle
	lease    *interactiveaccess.ExecutionLease
}

// ForSession binds a retained source configuration to its live human lease.
func (s *Service) ForSession(retained *apiv1.Gaggle, lease *interactiveaccess.ExecutionLease) (*SessionReader, error) {
	if s == nil || s.Backlog == nil || retained == nil || lease == nil {
		return nil, interactiveaccess.ErrDenied
	}
	if err := lease.AuthorizeSourceRead("backlog.read"); err != nil {
		return nil, err
	}
	if err := lease.RequireWorkbenchSources(retained); err != nil {
		return nil, err
	}
	return &SessionReader{service: s, retained: retained.DeepCopy(), lease: lease}, nil
}

// Get reads one item with the live session actor and source credential.
func (s *SessionReader) Get(ctx context.Context, binding string, request workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
	var result workbench.BacklogItem
	err := s.read(ctx, binding, func(ctx context.Context, r *workbenchprovider.BacklogReader) error {
		var err error
		result, err = r.Get(ctx, request)
		return err
	})
	return result, err
}

// Page reads one bounded window with the live session actor and credential.
func (s *SessionReader) Page(ctx context.Context, binding string, request workbench.BacklogPageRequest) (workbench.BacklogPage, error) {
	var result workbench.BacklogPage
	err := s.read(ctx, binding, func(ctx context.Context, r *workbenchprovider.BacklogReader) error {
		var err error
		result, err = r.Page(ctx, request)
		return err
	})
	return result, err
}
func (s *SessionReader) read(ctx context.Context, binding string, use func(context.Context, *workbenchprovider.BacklogReader) error) error {
	ctx, cancel := context.WithTimeout(ctx, workbenchprovider.MaxReadDuration)
	defer cancel()
	stop := context.AfterFunc(s.lease.Context(), cancel)
	defer stop()
	if err := s.lease.RequireWorkbenchSources(s.retained); err != nil {
		return publicReadError(err)
	}
	selected, err := selectBacklog(s.retained, binding)
	if err != nil {
		return publicReadError(err)
	}
	credential, err := s.lease.Credential(ctx, "backlog.read", interactiveaccess.Target{Kind: "backlog"})
	if err != nil {
		return publicReadError(err)
	}
	return publicReadError(s.service.useBacklog(ctx, selected, credential, use))
}
