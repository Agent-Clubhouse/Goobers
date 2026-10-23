// Package enginechildplane adapts the engine's generated-child custody owner
// to the HTTP engine child-admission plane. It exists so internal/httpapi
// never imports internal/engine (which imports internal/dispatcher, whose
// tests import httpapi); composition wires Plane into the API server.
package enginechildplane

import (
	"context"
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
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
	binding, err := p.Admissions.Admit(ctx, in)
	if err != nil {
		return nil, planeError(err)
	}
	return binding, nil
}

// Load decodes a binding and returns its exact digest-addressed manifest.
func (p Plane) Load(ctx context.Context, decode func(any) error) ([]byte, error) {
	var binding engine.ChildRunBinding
	if err := decode(&binding); err != nil {
		return nil, err
	}
	manifest, err := p.Admissions.Load(ctx, binding)
	return manifest, planeError(err)
}

// Cancel decodes a binding and cancels its child family.
func (p Plane) Cancel(ctx context.Context, decode func(any) error) error {
	var binding engine.ChildRunBinding
	if err := decode(&binding); err != nil {
		return err
	}
	return planeError(p.Admissions.Cancel(ctx, binding))
}

// planeError gives the worker client a status it can classify: 503 while no
// controller holds custody, 409 for a request that can never succeed. Other
// failures surface as a 500 without detail.
func planeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, engine.ErrChildCustodyUnavailable):
		return &httpapi.InterventionError{Status: http.StatusServiceUnavailable, Code: "child_custody_unavailable", Message: "generated-child custody is temporarily unavailable; retry", Err: err}
	case errors.Is(err, engine.ErrChildCustodyRefused):
		return &httpapi.InterventionError{Status: http.StatusConflict, Code: "child_custody_refused", Message: "generated-child custody refused the request", Err: err}
	default:
		return err
	}
}
