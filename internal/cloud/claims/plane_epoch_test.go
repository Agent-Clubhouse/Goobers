package claims

import (
	"context"

	"github.com/goobers/goobers/internal/claimrelease"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/localscheduler"
)

// epochClaims restates the epoch services' claims seam with literal
// parameter types, as a consumer outside this package declares it. Plane
// must satisfy it: a defined RecoveryGate or RunClosed type would not.
type epochClaims interface {
	AdoptAll(context.Context, *cloudlease.Handle) (int64, error)
	RenewRuns(context.Context, *cloudlease.Handle, map[string]bool) ([]localscheduler.ClaimEntry, error)
	RecoverExpired(context.Context, *cloudlease.Handle, interface{ RecoveryPermitted() bool },
		func(context.Context, string) (bool, error)) ([]localscheduler.ClaimEntry, error)
	ReleaseRun(context.Context, *cloudlease.Handle, string, claimrelease.Reason) ([]localscheduler.ClaimEntry, error)
}

var _ epochClaims = (*Plane)(nil)
