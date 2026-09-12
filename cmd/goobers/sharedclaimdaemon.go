package main

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

// coordinatedLedger reuses the freshly opened ledger under the API's existing
// cross-process lock. Reopening or acquiring another lock here would split the
// transaction or deadlock the claims plane.
func (s *daemonClaimService) coordinatedLedger(ledger *localscheduler.ClaimLedger) (*claimsclient.File, error) {
	return claimsclient.NewFile(claimsclient.FileConfig{
		LedgerPath: s.ledgerPath(),
		Shared:     s.shared,
		Open: func(string, ...localscheduler.LedgerOption) (claimsclient.FileLedger, error) {
			return ledger, nil
		},
	})
}

// The liveness probe runs before the claims lock is acquired; enumerate the
// current ledger under that lock so released or reassigned claims cannot be
// resurrected from the earlier probe snapshot.
func renewCoordinatedLiveClaims(ctx context.Context, layout instance.Layout, ledger *localscheduler.ClaimLedger, live map[string]bool, lease time.Duration, resolver claimsclient.SharedClaimResolver) ([]localscheduler.ClaimEntry, error) {
	service := &daemonClaimService{layout: layout, shared: resolver}
	var renewed []localscheduler.ClaimEntry
	for _, entry := range ledger.Snapshot() {
		if !live[entry.RunID] {
			continue
		}
		var ok bool
		var err error
		if entry.SharedDeadline.IsZero() {
			ok, err = ledger.RenewEntry(entry, lease)
		} else {
			request := httpapi.ClaimRequest{Gaggle: entry.Gaggle, Provider: entry.Provider, ItemID: entry.ExternalID, RunID: entry.RunID, Workflow: entry.Workflow}
			var response httpapi.ClaimResponse
			response, err = service.renewSharedClaim(ctx, ledger, request, entry, lease)
			ok = response.Ok
			if err == nil && !ok {
				err = fmt.Errorf("shared admission is no longer valid")
			}
		}
		if err != nil {
			return renewed, fmt.Errorf("renew claim %s for run %s: %w", entry.ItemID, entry.RunID, err)
		}
		if ok {
			renewed = append(renewed, entry)
		}
	}
	return renewed, nil
}

func (s *daemonClaimService) renewSharedClaim(ctx context.Context, ledger *localscheduler.ClaimLedger, request httpapi.ClaimRequest, entry localscheduler.ClaimEntry, lease time.Duration) (httpapi.ClaimResponse, error) {
	if entry.RunID != request.RunID || !entry.ExpiresAt.After(time.Now()) || !entry.SharedDeadline.After(time.Now()) {
		return httpapi.ClaimResponse{Ok: false, Holder: entry.RunID}, nil
	}
	if request.Workflow != "" && request.Workflow != entry.Workflow {
		return httpapi.ClaimResponse{}, fmt.Errorf("shared renewal does not match the owning workflow")
	}
	client, err := s.coordinatedLedger(ledger)
	if err != nil {
		return httpapi.ClaimResponse{}, err
	}
	ok, holder, err := client.ClaimScoped(ctx, claimKey(request), request.RunID, entry.Workflow, lease)
	if err != nil || !ok {
		return httpapi.ClaimResponse{Ok: ok, Holder: holder}, err
	}
	current, held := ledger.LookupScoped(claimKey(request))
	if !held || current.RunID != request.RunID {
		return httpapi.ClaimResponse{}, fmt.Errorf("renewed shared claim is no longer held")
	}
	return httpapi.ClaimResponse{Ok: true, ExpiresAt: &current.ExpiresAt}, nil
}
