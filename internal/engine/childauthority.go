package engine

import (
	"context"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpio"
)

// ChildAuthorityRequest asks the control plane for the child-workflow grant
// of a run's committed, active stage attempt (docs/design/
// engine-parent-child-authority.md §2). The worker names only the run and the
// committed origin; control derives everything else from state it holds.
// Endpoint is where the stage pod reaches control; it is not part of the
// grant.
type ChildAuthorityRequest struct {
	RunID    string                    `json:"runId"`
	Origin   apiv1.ChildWorkflowOrigin `json:"origin"`
	Endpoint string                    `json:"endpoint"`
}

// ChildAuthorityGrant is one bound grant, delivered only to the stage pod's
// child-workflow tool. BearerToken is a secret.
type ChildAuthorityGrant struct {
	Endpoint    string    `json:"endpoint"`
	BearerToken string    `json:"bearerToken"`
	GrantID     string    `json:"grantId"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// String redacts the grant from incidental formatted diagnostics.
func (g ChildAuthorityGrant) String() string { return "<child-workflow grant " + g.GrantID + ">" }

// GoString redacts the grant from Go-syntax formatted diagnostics.
func (g ChildAuthorityGrant) GoString() string { return g.String() }

// ChildAuthorityRelease revokes exactly the named grant when the stage
// activity ends.
type ChildAuthorityRelease struct {
	RunID   string                    `json:"runId"`
	Origin  apiv1.ChildWorkflowOrigin `json:"origin"`
	GrantID string                    `json:"grantId"`
}

// ChildWorkflowAuthority is the control-plane boundary the engine worker
// acquires and releases a stage attempt's child-workflow grant through. Both
// are idempotent: a lost acquire response re-delivers the same live grant,
// and releasing a grant already replaced or never bound is done. Errors carry
// the child-custody classes: ErrChildCustodyUnavailable is retried,
// ErrChildCustodyRefused is final.
type ChildWorkflowAuthority interface {
	AcquireChildAuthority(context.Context, ChildAuthorityRequest) (ChildAuthorityGrant, error)
	ReleaseChildAuthority(context.Context, ChildAuthorityRelease) error
}

// Validate checks the request's shape before it leaves the worker or is
// served by control.
func (r ChildAuthorityRequest) Validate() error {
	if !apiv1.ValidRunID(r.RunID) || r.Origin.StageOccurrence == "" || r.Origin.AttemptID == "" || !mcpio.ValidChildWorkflowEndpoint(r.Endpoint) {
		return errors.New("engine: invalid child authority request")
	}
	return nil
}

// Validate checks the release's shape.
func (r ChildAuthorityRelease) Validate() error {
	if !apiv1.ValidRunID(r.RunID) || r.Origin.StageOccurrence == "" || r.Origin.AttemptID == "" || r.GrantID == "" || len(r.GrantID) > 128 {
		return errors.New("engine: invalid child authority release")
	}
	return nil
}

// Access is the grant as the stage pod's child-workflow tool consumes it.
func (g ChildAuthorityGrant) Access(runID string) (*mcpio.ChildWorkflowAccess, error) {
	access := &mcpio.ChildWorkflowAccess{Endpoint: g.Endpoint, BearerToken: g.BearerToken}
	if err := access.Validate(runID); err != nil {
		return nil, err
	}
	return access, nil
}
