package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

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
