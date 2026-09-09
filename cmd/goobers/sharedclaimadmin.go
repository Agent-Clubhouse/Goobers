package main

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

func forceReleaseClaim(ctx context.Context, ledger *localscheduler.ClaimLedger, resolver claimsclient.SharedClaimResolver, entry localscheduler.ClaimEntry, actor string) error {
	if entry.SharedDeadline.IsZero() {
		return ledger.ForceReleaseEntry(entry, actor)
	}
	held, err := ledger.RevokeSharedExecution(claimsclient.KeyForEntry(entry), entry.SharedOwner, actor)
	if err != nil || !held {
		return err
	}
	if resolver == nil {
		return fmt.Errorf("shared force-release requires its provider resolver")
	}
	binding, err := resolver.Release(ctx, entry)
	if err != nil {
		return err
	}
	if binding.Owner != entry.SharedOwner {
		return sharedclaim.ErrNotOwner
	}
	return ledger.ForceReleaseCoordinatedShared(ctx, binding.Store, binding.RemoteKey, claimsclient.KeyForEntry(entry), entry.SharedOwner, actor)
}
