//go:build cloudintegration

package banditstore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/bandit/conformance"
	"github.com/goobers/goobers/internal/cloud/banditstore"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
)

func TestPostgresObservationStore(t *testing.T) {
	conformance.Run(t, func(t *testing.T) bandit.ObservationStore {
		_, _, s := acquireStore(t, testPool(t), "instance", "first")
		return s
	})
}

func TestObservationStoreFencesZombieAndRetriesLostResponse(t *testing.T) {
	pool := testPool(t)
	first, handle, s := acquireStore(t, pool, "instance", "first")
	r := conformance.Record("run-a", "build", 0, 1, 1)
	if err := s.Record(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	_, nextHandle, next := acquireStore(t, pool, "instance", "second")
	if err := next.Record(t.Context(), r); err != nil {
		t.Fatalf("lost-response retry across takeover: %v", err)
	}
	var epoch int64
	if err := pool.QueryRow(t.Context(), `SELECT owner_epoch FROM control.runner_bandit`).Scan(&epoch); err != nil || epoch != handle.Epoch {
		t.Fatalf("retry rewrote immutable outcome epoch: %d, %v", epoch, err)
	}
	r.Key.Attempt++
	if err := s.Record(t.Context(), r); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("zombie write = %v", err)
	}
	if _, err := s.Observations(t.Context(), "build"); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("zombie read = %v", err)
	}
	if err := next.Record(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	key, _, err := bandit.EncodeRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT owner_epoch FROM control.runner_bandit WHERE record_key=$1`, key).Scan(&epoch); err != nil || epoch != nextHandle.Epoch {
		t.Fatalf("successor epoch stamp: %d, %v", epoch, err)
	}
	got, err := next.Observations(t.Context(), "build")
	if err != nil || len(got) != 2 {
		t.Fatalf("takeover history = %v, %v", got, err)
	}
	_, _, isolated := acquireStore(t, pool, "other-instance", "first")
	if got, err := isolated.Observations(t.Context(), "build"); err != nil || len(got) != 0 {
		t.Fatalf("cross-instance sample leak = %v, %v", got, err)
	}
}

func TestFilePostgresObservationOrderingEquivalent(t *testing.T) {
	_, _, cloud := acquireStore(t, testPool(t), "instance", "first")
	local, err := bandit.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Equal assignment sequences deliberately exercise the backend tie-break.
	for _, runID := range []string{"run-c", "run-a", "run-b"} {
		r := conformance.Record(runID, "build", 0, 1, 1)
		for _, store := range []bandit.ObservationStore{cloud, local} {
			if err := store.Record(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}
	}
	want, err := local.Observations(t.Context(), "build")
	if err != nil {
		t.Fatal(err)
	}
	got, err := cloud.Observations(t.Context(), "build")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("cloud %v != local %v: %v", got, want, err)
	}
}

func acquireStore(t *testing.T, pool *pgxpool.Pool, instance, holder string) (*lease.Manager, *lease.Handle, *banditstore.Store) {
	t.Helper()
	m, err := lease.New(lease.Config{Pool: pool, InstanceID: instance, Name: "controller", Holder: holder, TTL: time.Minute, SelfFenceAfter: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h, err := m.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s, err := banditstore.New(m, h)
	if err != nil {
		t.Fatal(err)
	}
	return m, h, s
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("bandit_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	config.MaxConns = lease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := migrate.Runner{Direct: pool, Catalog: catalog, BackupRef: "bandit-test", InstanceID: "migration", Holder: "test"}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool
}
