package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

func TestClaimVerificationServiceContainsPodsAndRejectsBadObservations(t *testing.T) {
	service, layout := newClaimServiceFixture(t)
	ctx := context.Background()
	if response, err := service.Acquire(ctx, claimPlaneRequest("owner")); err != nil || !response.Ok {
		t.Fatalf("acquire: %+v %v", response, err)
	}
	listing, err := service.List(ctx, httpapi.ClaimListRequest{RunID: "owner", Scope: httpapi.ClaimListScopeRun})
	if err != nil || len(listing.Entries) != 1 {
		t.Fatalf("list: %+v %v", listing, err)
	}
	entry := listing.Entries[0]
	if entry.Verification.State != "unverified" {
		t.Fatalf("unobserved claim reported checked: %+v", entry)
	}
	run, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{RunID: "observer", Workflow: "reconcile", Gaggle: "example"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	request := httpapi.ClaimVerificationRequest{RunID: "observer", Gaggle: "example", Provider: "github", ItemID: "42", OwnerRunID: "owner", ClaimedAt: entry.ClaimedAt, PodScoped: true, Observation: httpapi.ClaimVerification{State: "verified", ObservedAt: time.Now().UTC(), ProviderRunID: "owner"}}
	for name, mutate := range map[string]func(*httpapi.ClaimVerificationRequest){
		"unknown state":            func(r *httpapi.ClaimVerificationRequest) { r.Observation.State = "invented" },
		"contradictory owner":      func(r *httpapi.ClaimVerificationRequest) { r.Observation.ProviderRunID = "other" },
		"future observation":       func(r *httpapi.ClaimVerificationRequest) { r.Observation.ObservedAt = time.Now().Add(time.Hour) },
		"oversized provider owner": func(r *httpapi.ClaimVerificationRequest) { r.Observation.ProviderRunID = strings.Repeat("x", 1025) },
		"missing caller":           func(r *httpapi.ClaimVerificationRequest) { r.RunID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := request
			mutate(&bad)
			_, err := service.RecordVerification(ctx, bad)
			var refusal *httpapi.InterventionError
			if !errors.As(err, &refusal) || refusal.Status != http.StatusBadRequest {
				t.Fatalf("bad observation not 400: %v", err)
			}
		})
	}
	wrong := request
	wrong.Gaggle = "other"
	_, err = service.RecordVerification(ctx, wrong)
	var refusal *httpapi.InterventionError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
		t.Fatalf("cross-gaggle observation accepted: %v", err)
	}
	if response, err := service.RecordVerification(ctx, request); err != nil || !response.Ok {
		t.Fatalf("same-gaggle observer: %+v %v", response, err)
	}
	listing, err = service.List(ctx, httpapi.ClaimListRequest{RunID: "owner", Scope: httpapi.ClaimListScopeRun, IncludeHistory: true})
	if err != nil || listing.Entries[0].Verification != request.Observation || !listing.Entries[0].ExpiresAt.Equal(entry.ExpiresAt) {
		t.Fatalf("verification not persisted independently of lease: %+v %v", listing, err)
	}
}
