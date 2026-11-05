package main

import (
	"context"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// Called outside the claims lock. Revocation itself precedes provider setup or
// waiting for a pending transition; only its exact persisted owner is cleaned.
func forceReleaseClaim(ctx context.Context, layout instance.Layout, log *journal.InstanceLog, resolver claimsclient.SharedClaimResolver, entry localscheduler.ClaimEntry, actor string) error {
	service := newBareDaemonClaimService(layout, log, resolver)
	client, err := service.coordinatedLedger(claimLockOperationAdminRelease, httpapi.ClaimRequest{RunID: entry.RunID, Gaggle: entry.Gaggle})
	if err != nil {
		return err
	}
	return client.ForceReleaseExpected(ctx, entry, actor)
}
