//go:build cloudintegration

package webhookintake

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/webhook"
)

const instance = "webhook-instance"

// replica is one API replica: its own pool connected as goobers_api and no
// controller credentials or lease.
func replica(t *testing.T, owner *pgxpool.Pool) (*Intake, *pgxpool.Pool) {
	t.Helper()
	config := owner.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_api")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var current string
	if err := pool.QueryRow(t.Context(), "SELECT current_user").Scan(&current); err != nil || current != "goobers_api" {
		t.Fatalf("current_user = %q, %v; want goobers_api", current, err)
	}
	store, err := acceptance.New(pool, instance)
	if err != nil {
		t.Fatal(err)
	}
	return testIntake(store), pool
}

func count(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), query, instance).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func delivery(id string) webhook.Delivery {
	return webhook.Delivery{Event: "pull_request", ID: id, RepositoryOwner: "acme", RepositoryName: "app", PullNumber: 7, PayloadDigest: "d"}
}

func TestWebhookIntakeUnderAPIRoleGrants(t *testing.T) {
	owner := pgtest.Pool(t, "webhook")
	first, apiPool := replica(t, owner)
	second, _ := replica(t, owner)

	// Concurrent redelivery to two replicas: one receipt, the same acceptances.
	const attempts = 16
	results := make([][]string, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			intake := first
			if i%2 == 1 {
				intake = second
			}
			results[i], errs[i] = intake.AcceptWebhook(t.Context(), delivery("delivery-1"), time.Now())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	for i := 1; i < attempts; i++ {
		if !slices.Equal(results[0], results[i]) {
			t.Fatalf("attempt %d acceptance IDs %v differ from %v", i, results[i], results[0])
		}
	}
	if len(results[0]) != 2 {
		t.Fatalf("acceptance IDs = %v, want one per subscribed workflow", results[0])
	}
	if n := count(t, apiPool, "SELECT count(*) FROM control.source_receipt WHERE instance_id=$1"); n != 1 {
		t.Fatalf("source receipts = %d, want 1", n)
	}
	if n := count(t, apiPool, "SELECT count(*) FROM control.acceptance WHERE instance_id=$1"); n != 2 {
		t.Fatalf("acceptances = %d, want 2", n)
	}

	// A different delivery is a different receipt.
	if _, err := second.AcceptWebhook(t.Context(), delivery("delivery-2"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, apiPool, "SELECT count(*) FROM control.source_receipt WHERE instance_id=$1"); n != 2 {
		t.Fatalf("source receipts = %d, want 2", n)
	}

	// Reusing a delivery ID for a different payload is an identity conflict.
	conflicting := delivery("delivery-1")
	conflicting.PayloadDigest = "other"
	if _, err := first.AcceptWebhook(t.Context(), conflicting, time.Now()); err == nil {
		t.Fatal("conflicting reuse of a delivery ID was accepted")
	}

	// The API role holds insert and select on receipts, never update or delete.
	for _, statement := range []string{
		"UPDATE control.source_receipt SET actor='x'",
		"DELETE FROM control.source_receipt",
	} {
		_, err := apiPool.Exec(t.Context(), statement)
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "42501" {
			t.Fatalf("%q: want insufficient_privilege, got %v", statement, err)
		}
	}
}

func TestUnauthenticatedDeliveryPersistsNothing(t *testing.T) {
	owner := pgtest.Pool(t, "webhookauth")
	intake, apiPool := replica(t, owner)
	handler := newTestHandler(t, intake)
	if rec := deliver(t, handler, sign([]byte("wrong"), body), "delivery-bad"); rec.Code != 401 {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if n := count(t, apiPool, "SELECT count(*) FROM control.source_receipt WHERE instance_id=$1"); n != 0 {
		t.Fatalf("receipts = %d after a failed authentication", n)
	}
	if n := count(t, apiPool, "SELECT count(*) FROM control.acceptance WHERE instance_id=$1"); n != 0 {
		t.Fatalf("acceptances = %d after a failed authentication", n)
	}
	if rec := deliver(t, handler, sign(testSecret, body), "delivery-good"); rec.Code != 202 {
		t.Fatalf("status %d, want 202", rec.Code)
	}
	if n := count(t, apiPool, "SELECT count(*) FROM control.source_receipt WHERE instance_id=$1"); n != 1 {
		t.Fatalf("receipts = %d, want 1", n)
	}
}
