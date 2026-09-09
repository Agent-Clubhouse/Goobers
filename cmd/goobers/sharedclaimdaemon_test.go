package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

type failingReleaseClaimStore struct {
	pinnedClaimTestStore
	failRelease bool
	failRenew   bool
}

func (s *failingReleaseClaimStore) CompareAndSwap(ctx context.Context, key, revision string, record sharedclaim.Record) error {
	if s.failRenew && record.Owner != (sharedclaim.Owner{}) {
		return errors.New("provider renewal unavailable")
	}
	if s.failRelease && record.Owner == (sharedclaim.Owner{}) {
		return errors.New("provider cleanup unavailable")
	}
	return s.pinnedClaimTestStore.CompareAndSwap(ctx, key, revision, record)
}

func TestLiveClaimRenewalRequiresProviderAckAndNeverResurrectsRelease(t *testing.T) {
	layout, _ := newPinnedClaimResolverRun(t, "shared")
	store := &failingReleaseClaimStore{}
	resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
		return store, nil
	}}
	service := newDaemonClaimService(layout, nil, nil)
	service.shared = resolver
	request := httpapi.ClaimRequest{Gaggle: "example", Provider: "github", ItemID: "42", RunID: "shared-run", Workflow: "claim", LeaseSeconds: 60}
	if response, err := service.Acquire(t.Context(), request); err != nil || !response.Ok {
		t.Fatalf("acquire: %+v %v", response, err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if renewed, err := renewCoordinatedLiveClaims(t.Context(), layout, ledger, nil, time.Minute, resolver); err != nil || len(renewed) != 0 || store.writes != 1 {
		t.Fatalf("unprobed holder renewed: %+v %v", renewed, err)
	}
	live := map[string]bool{request.RunID: true}
	if renewed, err := renewCoordinatedLiveClaims(t.Context(), layout, ledger, live, time.Minute, resolver); err != nil || len(renewed) != 1 || store.writes != 2 {
		t.Fatalf("live renewal: %+v %v", renewed, err)
	}
	before, _ := ledger.LookupScoped(claimKey(request))
	store.failRenew = true
	if renewed, err := renewCoordinatedLiveClaims(t.Context(), layout, ledger, live, time.Minute, resolver); err == nil || len(renewed) != 0 {
		t.Fatalf("unacknowledged renewal succeeded: %+v %v", renewed, err)
	}
	after, held := ledger.LookupScoped(claimKey(request))
	if !held || !after.ExpiresAt.Equal(before.ExpiresAt) || !after.SharedDeadline.Equal(before.SharedDeadline) {
		t.Fatal("failed provider renewal extended local execution authority")
	}
	client, err := service.coordinatedLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseScoped(t.Context(), claimKey(request), request.RunID); err != nil {
		t.Fatal(err)
	}
	if renewed, err := renewCoordinatedLiveClaims(t.Context(), layout, ledger, live, time.Minute, resolver); err != nil || len(renewed) != 0 || store.writes != 3 {
		t.Fatalf("stale probe resurrected released claim: %+v %v", renewed, err)
	}
}

func TestHeldLifecycleLedgerRetainsSharedOwnershipUntilReleaseAcknowledged(t *testing.T) {
	layout, run := newPinnedClaimResolverRun(t, "shared")
	store := &failingReleaseClaimStore{}
	resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
		return store, nil
	}}
	service := newDaemonClaimService(layout, nil, nil)
	service.shared = resolver
	request := httpapi.ClaimRequest{Gaggle: "example", Provider: "github", ItemID: "42", RunID: "shared-run", Workflow: "claim", LeaseSeconds: 60}
	if response, err := service.Acquire(t.Context(), request); err != nil || !response.Ok {
		t.Fatalf("acquire: %+v %v", response, err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	ledger, err := heldClaimLedgerWithResolver(layout, resolver)
	if err != nil {
		t.Fatal(err)
	}
	store.failRelease = true
	if released, err := ledger.ReleaseAllForRun(t.Context(), request.RunID); err == nil || len(released) != 0 {
		t.Fatalf("unacknowledged release succeeded: %+v %v", released, err)
	}
	if entries, err := ledger.ForRunAll(t.Context(), request.RunID); err != nil || len(entries) != 1 || store.record.Owner.Run != request.RunID {
		t.Fatalf("failed cleanup discarded custody: %+v %v", entries, err)
	}
	store.failRelease = false
	if released, err := ledger.ReleaseAllForRun(t.Context(), request.RunID); err != nil || len(released) != 1 || store.record.Owner != (sharedclaim.Owner{}) {
		t.Fatalf("acknowledged release: %+v %v", released, err)
	}
}

func TestDaemonSharedClaimAcquireRenewAndTerminalRelease(t *testing.T) {
	layout, run := newPinnedClaimResolverRun(t, "shared")
	store := &pinnedClaimTestStore{}
	service := newDaemonClaimService(layout, nil, nil)
	service.shared = pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
		return store, nil
	}}
	request := httpapi.ClaimRequest{Gaggle: "example", Provider: "github", ItemID: "42", RunID: "shared-run", Workflow: "claim", LeaseSeconds: 60}
	response, err := service.Acquire(t.Context(), request)
	if err != nil || !response.Ok || response.ExpiresAt == nil || store.writes != 1 {
		t.Fatalf("acquire: %+v %v writes=%d", response, err, store.writes)
	}
	if response.ExpiresAt.After(store.record.ExpiresAt) {
		t.Fatal("API lease outlives provider lease")
	}
	forged := request
	forged.Workflow = "other"
	if _, err := service.Renew(t.Context(), forged); err == nil || store.writes != 1 {
		t.Fatal("forged workflow renewed a shared lease")
	}
	response, err = service.Renew(t.Context(), request)
	if err != nil || !response.Ok || response.ExpiresAt == nil || store.writes != 2 || response.ExpiresAt.After(store.record.ExpiresAt) {
		t.Fatalf("renew: %+v %v writes=%d", response, err, store.writes)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(t.Context(), request); err == nil || store.writes != 2 {
		t.Fatal("terminal run renewed a shared lease")
	}
	response, err = service.Release(t.Context(), request)
	if err != nil || !response.Ok || store.record.Owner != (sharedclaim.Owner{}) || store.writes != 3 {
		t.Fatalf("terminal release: %+v %v writes=%d", response, err, store.writes)
	}
	if _, err := service.Release(t.Context(), request); err != nil || store.writes != 3 {
		t.Fatalf("duplicate release mutated provider: %v writes=%d", err, store.writes)
	}
}
