package enginechildplane

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/engine"
)

// Authority serves httpapi.EngineChildAuthority over an
// engine.ChildWorkflowAuthority: the control half of a stage attempt's
// child-workflow grant acquire and release.
type Authority struct {
	Authorities engine.ChildWorkflowAuthority
}

// NewAuthority returns the authority plane adapter.
func NewAuthority(authorities engine.ChildWorkflowAuthority) (Authority, error) {
	if authorities == nil {
		return Authority{}, errors.New("engine child plane: child workflow authority is required")
	}
	return Authority{Authorities: authorities}, nil
}

// Acquire decodes an authority request and returns the bound grant.
func (a Authority) Acquire(ctx context.Context, decode func(any) error) (any, error) {
	var request engine.ChildAuthorityRequest
	if err := decode(&request); err != nil {
		return nil, err
	}
	grant, err := a.Authorities.AcquireChildAuthority(ctx, request)
	if err != nil {
		return nil, planeError(err)
	}
	return grant, nil
}

// Release decodes a release and revokes exactly the named grant.
func (a Authority) Release(ctx context.Context, decode func(any) error) error {
	var release engine.ChildAuthorityRelease
	if err := decode(&release); err != nil {
		return err
	}
	return planeError(a.Authorities.ReleaseChildAuthority(ctx, release))
}

// Observe returns only a handoff reconstructed from control-owned custody.
func (a Authority) Observe(ctx context.Context, decode func(any) error) (any, error) {
	var request engine.ChildHandoffObservation
	if err := decode(&request); err != nil {
		return nil, err
	}
	observer, ok := a.Authorities.(engine.ChildHandoffObserver)
	if !ok {
		return nil, planeError(engine.ErrChildCustodyUnavailable)
	}
	handoff, err := observer.ObserveChildHandoff(ctx, request)
	return handoff, planeError(err)
}
