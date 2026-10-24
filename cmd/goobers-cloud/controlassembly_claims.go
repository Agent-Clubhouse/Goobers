package main

import (
	"context"
	"errors"
	"time"

	cloudclaims "github.com/goobers/goobers/internal/cloud/claims"
	"github.com/goobers/goobers/internal/cloud/lease"
)

// cloudDefaultClaimLease is the lease a claims request without leaseSeconds
// receives: the daemon's DefaultClaimLease, of which
// httpapi.MaxClaimLeaseSeconds is the 4x ceiling.
const cloudDefaultClaimLease = 30 * time.Minute

// constructClaims composes the claims plane on the controller role: claim
// writes are lease-fenced, and control.claim is granted to goobers_controller
// only (writepaths.yaml), even when a pod's API request initiates them.
func constructClaims(_ context.Context, db databaseServices, _ controlAuth, manager *lease.Manager, owner func() *lease.Handle, p *controlPlanes) error {
	if db.Pools == nil || db.InstanceID == "" {
		return errors.New("control claims require the controller database and instance")
	}
	if manager == nil || owner == nil {
		return errors.New("control claims require the controller lease")
	}
	if db.Pools.Transaction == nil {
		return errors.New("control claims requires a pool")
	}
	provider, err := newControlProvider(db, p)
	if err != nil {
		return err
	}
	plane, err := cloudclaims.NewPlane(cloudclaims.PlaneConfig{
		Pool: db.Pools.Transaction, InstanceID: db.InstanceID,
		Lease: manager, Owner: owner, DefaultLease: cloudDefaultClaimLease,
		ReleaseMarker: provider.writer.ReleaseClaim,
	})
	if err != nil {
		return err
	}
	p.provider = provider
	p.Claims = plane
	p.claimedItems = plane.ClaimedItems
	return nil
}
