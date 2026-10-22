package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

// coordinatedLedger owns each short claims-lock section. It must be created
// and called outside any caller-owned claims lock; provider transitions use
// the separate per-item lock and reopen claims.json before each CAS.
func (s *daemonClaimService) coordinatedLedger(operation string, request httpapi.ClaimRequest) (*claimsclient.File, error) {
	return claimsclient.NewFile(claimsclient.FileConfig{
		LedgerPath: s.ledgerPath(), Shared: s.shared,
		Options: withClaimJournal(s.log), ProviderReleaseLock: claimProviderReleaseLock(s.layout),
		Lock: func(name string, fn func() error) error {
			if strings.HasPrefix(name, "claims.client.") {
				name = operation
			}
			return withClaimLockForRun(s.lockPath(), name, request.Gaggle, request.RunID, fn)
		},
	})
}

// Probe snapshots are hints only. RenewExpected compares the exact persisted
// incarnation again under the claims lock and cannot resurrect a release.
func renewCoordinatedLiveClaims(ctx context.Context, layout instance.Layout, entries []localscheduler.ClaimEntry, live map[string]bool, lease time.Duration, resolver claimsclient.SharedClaimResolver) ([]localscheduler.ClaimEntry, error) {
	service := newBareDaemonClaimService(layout, nil, resolver)
	client, err := service.coordinatedLedger(claimLockOperationRenewal, httpapi.ClaimRequest{})
	if err != nil {
		return nil, err
	}
	var renewed []localscheduler.ClaimEntry
	for _, entry := range entries {
		if !live[entry.RunID] || entry.IsPublicationClaim() {
			continue
		}
		ok, err := client.RenewExpected(ctx, entry, lease)
		if err != nil {
			return renewed, fmt.Errorf("renew claim %s for run %s: %w", entry.ItemID, entry.RunID, err)
		}
		if ok {
			renewed = append(renewed, entry)
		}
	}
	return renewed, nil
}

func (s *daemonClaimService) renewSharedClaim(ctx context.Context, request httpapi.ClaimRequest, entry localscheduler.ClaimEntry, lease time.Duration) (httpapi.ClaimResponse, error) {
	if entry.RunID != request.RunID || !entry.ExpiresAt.After(time.Now()) || !entry.SharedDeadline.After(time.Now()) {
		return httpapi.ClaimResponse{Ok: false, Holder: entry.RunID}, nil
	}
	if request.Workflow != "" && request.Workflow != entry.Workflow {
		return httpapi.ClaimResponse{}, fmt.Errorf("shared renewal does not match the owning workflow")
	}
	client, err := s.coordinatedLedger(claimLockOperationAPIRenew, request)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	ok, err := client.RenewExpected(ctx, entry, lease)
	if err != nil || !ok {
		return httpapi.ClaimResponse{Ok: ok, Holder: entry.RunID}, err
	}
	return s.currentAdmissionResponse(ctx, client, request)
}

func (s *daemonClaimService) currentAdmissionResponse(ctx context.Context, client *claimsclient.File, request httpapi.ClaimRequest) (httpapi.ClaimResponse, error) {
	listing, err := client.ListNamespace(ctx, request.Gaggle, request.Provider)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	entry, held := listing.Lookup(claimKey(request))
	if !held || entry.RunID != request.RunID || entry.ReleasePending || entry.SharedAdmissionPending {
		return httpapi.ClaimResponse{Ok: false, Holder: entry.RunID}, nil
	}
	return httpapi.ClaimResponse{Ok: true, ExpiresAt: &entry.ExpiresAt}, nil
}
