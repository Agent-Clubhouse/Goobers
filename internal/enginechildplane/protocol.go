package enginechildplane

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
)

// ChildProtocol is the part of engine.ChildService the protocol routes serve:
// parking a parent on its child, sealing the child's result, and resuming
// the parent with it (#8085 d).
type ChildProtocol interface {
	Park(context.Context, engine.ChildParkInput) error
	Finalize(context.Context, engine.ChildFinalizeInput) (childresult.TerminalReturn, error)
	Complete(context.Context, engine.ChildCompletionInput) (apiv1.ContextPointer, error)
}

// Protocol serves httpapi.EngineChildProtocol over a ChildProtocol.
type Protocol struct {
	Children ChildProtocol
}

// NewProtocol returns the protocol plane adapter.
func NewProtocol(children ChildProtocol) (Protocol, error) {
	if children == nil {
		return Protocol{}, errors.New("engine child plane: the child protocol is required")
	}
	return Protocol{Children: children}, nil
}

// Park decodes a park input and lends the parked parent's capacity.
func (p Protocol) Park(ctx context.Context, decode func(any) error) error {
	var in engine.ChildParkInput
	if err := decode(&in); err != nil {
		return err
	}
	return planeError(p.Children.Park(ctx, in))
}

// Finalize decodes a finalize input and returns the sealed child result.
func (p Protocol) Finalize(ctx context.Context, decode func(any) error) (any, error) {
	var in engine.ChildFinalizeInput
	if err := decode(&in); err != nil {
		return nil, err
	}
	result, err := p.Children.Finalize(ctx, in)
	if err != nil {
		return nil, planeError(err)
	}
	return result, nil
}

// Complete decodes a completion input and returns the parent's continuation.
func (p Protocol) Complete(ctx context.Context, decode func(any) error) (any, error) {
	var in engine.ChildCompletionInput
	if err := decode(&in); err != nil {
		return nil, err
	}
	pointer, err := p.Children.Complete(ctx, in)
	if err != nil {
		return nil, planeError(err)
	}
	return pointer, nil
}
