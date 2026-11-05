package claims

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/localscheduler"
)

func storeReads(s *Store) map[string]func(context.Context) ([]localscheduler.ClaimEntry, error) {
	return map[string]func(context.Context) ([]localscheduler.ClaimEntry, error){
		"active":  s.SnapshotErr,
		"history": s.HistorySnapshotErr,
		"run":     func(ctx context.Context) ([]localscheduler.ClaimEntry, error) { return s.ForRunAllErr(ctx, "run-a") },
	}
}

func TestStoreClaimReadsNeverTurnClosedPoolIntoEmptyClaims(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close() // No connection or external service is needed to fail reads.
	store := &Store{pool: pool, instanceID: "instance"}
	for name, read := range storeReads(store) {
		got, err := read(t.Context())
		if !errors.Is(err, localscheduler.ErrClaimRead) || got != nil {
			t.Errorf("%s=%+v, %v; want no snapshot and claim read error", name, got, err)
		}
	}
	plane := &Plane{pool: pool, instanceID: "instance"}
	got, err := plane.ListNamespace(t.Context(), "g", "github")
	if !errors.Is(err, localscheduler.ErrClaimRead) || got.Entries != nil || got.History != nil {
		t.Fatalf("namespace=%+v, %v", got, err)
	}
}

func TestStoreClaimReadDeadlineKeepsItsCauseAndSafetyMarker(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := &Store{pool: pool, instanceID: "instance"}
	ctx, cancel := context.WithDeadline(t.Context(), time.Time{})
	defer cancel()
	for name, read := range storeReads(store) {
		got, err := read(ctx)
		if got != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, localscheduler.ErrClaimRead) {
			t.Errorf("%s=%+v, %v", name, got, err)
		}
	}
}
