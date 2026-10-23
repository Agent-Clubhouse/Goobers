// Package enginechildplane adapts the engine's generated-child custody owner
// to the HTTP engine child-admission plane. It exists so internal/httpapi
// never imports internal/engine (which imports internal/dispatcher, whose
// tests import httpapi); composition wires Plane into the API server.
package enginechildplane

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/engine"
)

// Plane serves httpapi.EngineChildPlane over an engine.ChildAdmissions.
type Plane struct {
	Admissions engine.ChildAdmissions
}

// New returns the plane adapter for admissions.
func New(admissions engine.ChildAdmissions) (Plane, error) {
	if admissions == nil {
		return Plane{}, errors.New("engine child plane: admissions are required")
	}
	return Plane{Admissions: admissions}, nil
}

// Admit decodes an admission input and returns the custody binding.
func (p Plane) Admit(ctx context.Context, decode func(any) error) (any, error) {
	var in engine.ChildAdmissionInput
	if err := decode(&in); err != nil {
		return nil, err
	}
	return p.Admissions.Admit(ctx, in)
}

// Load decodes a binding and returns its exact digest-addressed manifest.
func (p Plane) Load(ctx context.Context, decode func(any) error) ([]byte, error) {
	var binding engine.ChildRunBinding
	if err := decode(&binding); err != nil {
		return nil, err
	}
	return p.Admissions.Load(ctx, binding)
}

// Cancel decodes a binding and cancels its child family.
func (p Plane) Cancel(ctx context.Context, decode func(any) error) error {
	var binding engine.ChildRunBinding
	if err := decode(&binding); err != nil {
		return err
	}
	return p.Admissions.Cancel(ctx, binding)
}
