package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
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

// Provider observations are composed by default: the epoch refuses naming only
// what is still missing, never the observations.
func TestEpochWiringComposesProviderObservations(t *testing.T) {
	if epochWiring.Observations == nil {
		t.Fatal("provider observations are not composed into the epoch wiring")
	}
	if missing := epochWiring.missing(); len(missing) != 1 || missing[0] != "claims epoch maintenance" {
		t.Fatalf("missing = %v, want only the claim plane", missing)
	}
	observations := epochWiring.Observations()
	if observations.Restore == nil || observations.Capture == nil {
		t.Fatal("observation restore/capture not wired")
	}
	if _, err := observations.Capture(context.Background()); err != nil {
		t.Fatalf("capture over the shared quota ledger: %v", err)
	}
}
