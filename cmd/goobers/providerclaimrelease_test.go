package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestClaimsPlaneProviderReleaseRetainsCustodyAndRetries(t *testing.T) {
	service, layout := newClaimServiceFixture(t)
	ctx := context.Background()
	request := claimPlaneRequest("run")
	if response, err := service.Acquire(ctx, request); err != nil || !response.Ok {
		t.Fatalf("claim: %+v %v", response, err)
	}
	local, err := fileClaimLedger(layout)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := local.ForRunAll(ctx, "run")
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %v %v", entries, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request httpapi.ClaimRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		response, err := service.Release(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	client, err := claimsclient.NewHTTP(claimsclient.HTTPConfig{BaseURL: server.URL, Token: "test", RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	failed := true
	calls := 0
	service.markerRelease = func(ctx context.Context, entry localscheduler.ClaimEntry, reason claimrelease.Reason) error {
		calls++
		// This synchronously takes the real claims lock inside provider I/O.
		// It would time out if the service still held that lock.
		if _, _, err := local.ClaimScoped(ctx, claimsclient.Key{ExternalID: "other"}, "other-run", "implementation", time.Minute); err != nil {
			return err
		}
		if entry.RunID != "run" || entry.ItemID != "42" || reason.Code != workflowReleaseStepReason.Code {
			return errors.New("cleanup lost request identity")
		}
		if failed {
			return errors.New("provider unavailable")
		}
		return nil
	}
	unused := func(context.Context) error { return errors.New("client must not perform provider I/O") }
	if ok, err := client.ReleaseWithProvider(ctx, entries[0], workflowReleaseStepReason, unused); ok || err == nil {
		t.Fatalf("failed cleanup: %t %v", ok, err)
	}
	pending, _ := local.ForRunAll(ctx, "run")
	if len(pending) != 1 || !pending[0].ReleasePending {
		t.Fatalf("lost retry custody: %+v", pending)
	}
	if response, err := service.Acquire(ctx, claimPlaneRequest("successor")); err != nil || response.Ok {
		t.Fatalf("admitted successor during pending cleanup: %+v %v", response, err)
	}
	failed = false
	if ok, err := client.ReleaseWithProvider(ctx, entries[0], workflowReleaseStepReason, unused); !ok || err != nil {
		t.Fatalf("retry cleanup: %t %v", ok, err)
	}
	if ok, err := client.ReleaseWithProvider(ctx, entries[0], workflowReleaseStepReason, unused); ok || err != nil {
		t.Fatalf("stale retry: %t %v", ok, err)
	}
	if calls != 2 {
		t.Fatalf("cleanup calls = %d, want failure + retry", calls)
	}
}
