package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestEpochSeamIsFailClosedUntilItsPartsAreComposed(t *testing.T) {
	var planes controlPlanes
	if err := constructEpoch(context.Background(), databaseServices{}, controlAuth{}, nil, &planes); err == nil {
		t.Fatal("epoch seam composed without the controller's direct pool")
	}
	db := databaseServices{Pools: &cloudpg.Pools{Direct: &pgxpool.Pool{}}}
	if err := constructEpoch(context.Background(), db, controlAuth{}, nil, &planes); err != nil {
		t.Fatal(err)
	}
	if planes.Epoch == nil {
		t.Fatal("constructor did not compose the Epoch seam")
	}
	original := epochWiring
	t.Cleanup(func() { epochWiring = original })
	epochWiring = epochWiringParts{}
	_, err := planes.Epoch(context.Background(), &lease.Handle{InstanceID: "i", Name: lease.ControllerName, Holder: "h", Epoch: 1})
	if err == nil || !strings.Contains(err.Error(), "claims epoch maintenance") || !strings.Contains(err.Error(), "provider observations") {
		t.Fatalf("Epoch with nothing composed = %v, want a refusal naming the missing parts", err)
	}
}

// Provider observations stay explicitly PENDING until the L1 scheduler binds a
// producer: the seam must refuse, not silently persist an empty ledger.
func TestEpochWiringProviderObservationsArePendingUntilAProducerBinds(t *testing.T) {
	original := epochWiring
	t.Cleanup(func() { epochWiring = original })
	if epochWiring.Observations != nil {
		t.Fatal("provider observations are wired with no producer; an empty ledger would be persisted silently")
	}
	if missing := epochWiring.missing(); !slices.Contains(missing, "provider observations") {
		t.Fatalf("missing = %v, want provider observations named", missing)
	}
	// Binding a real ledger composes the seam; the persisted state IS the one the
	// scheduler records into (same pointer, not a copy).
	state := localscheduler.NewProviderQuotaState()
	epochWiring.Observations = observationsFor(state, nil)
	if missing := epochWiring.missing(); slices.Contains(missing, "provider observations") {
		t.Fatalf("missing = %v after binding", missing)
	}
	reset := time.Now().Add(time.Hour)
	state.Record(apiv1.ProviderGitHub, 12, reset)
	snapshot, err := epochWiring.Observations().Capture(context.Background())
	if err != nil || len(snapshot) != 1 || snapshot[0].Key != "provider-quota/github" {
		t.Fatalf("capture = %+v %v, want the window recorded into the scheduler's own state", snapshot, err)
	}
	successor := localscheduler.NewProviderQuotaState()
	if err := observationsFor(successor, nil)().Restore(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if budget := successor.ReserveCurrentPolls(apiv1.ProviderGitHub, 100); budget.Allowed != 12 {
		t.Fatalf("restored budget = %d, want 12", budget.Allowed)
	}
}
