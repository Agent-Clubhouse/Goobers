//go:build cloudintegration

package acceptance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
)

func TestPGStartCustodyUsesInventoryControllerGrants(t *testing.T) {
	_, owner := testStore(t, 1)
	rolePool := func(role string) *pgxpool.Pool {
		config := owner.Config()
		config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
			return err
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		var currentUser string
		if err := pool.QueryRow(t.Context(), "SELECT current_user").Scan(&currentUser); err != nil {
			t.Fatal(err)
		}
		if currentUser != role {
			t.Fatalf("current_user=%q, want %q", currentUser, role)
		}
		return pool
	}
	apiPool := rolePool("goobers_api")
	api, err := New(apiPool, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	controllerPool := rolePool("goobers_controller")
	controller, err := New(controllerPool, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	acquire := func(holder string) (*lease.Manager, *lease.Handle) {
		manager, err := lease.New(lease.Config{Pool: controllerPool, InstanceID: "test-instance", Name: lease.ControllerName, Holder: holder})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := manager.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		controller.BindFence(manager, handle)
		return manager, handle
	}
	first, handle := acquire("first")
	receipt, _, err := api.Accept(t.Context(), "role-start", "actor", []byte("payload"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE control.acceptance SET dispatch_started_at=clock_timestamp() WHERE id=$1",
		"UPDATE control.acceptance SET start_retry_count=1 WHERE id=$1",
		"SELECT start_retry_count FROM control.acceptance WHERE id=$1",
	} {
		_, err := apiPool.Exec(t.Context(), statement, receipt.ID)
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "42501" {
			t.Fatalf("API query %q: want insufficient_privilege, got %v", statement, err)
		}
	}
	custody := &startCustody{store: controller}
	identity, err := custody.ReadStart(t.Context(), receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.WithFence(t.Context(), handle, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		err := tx.QueryRow(ctx, `UPDATE control.acceptance
   SET dispatch_started_at=clock_timestamp()-interval '2 minutes', start_retry_count=0
   WHERE id=$1 RETURNING start_retry_count`, receipt.ID).Scan(&count)
		if err == nil && count != 0 {
			t.Fatalf("controller retry count=%d, want 0", count)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retries, err := controller.reserveStartRetries(t.Context())
	if err != nil || len(retries) != 1 {
		t.Fatalf("controller retry: %v %v", retries, err)
	}
	if statuses, err := api.StartStatusPage(t.Context(), "", 10); err != nil || len(statuses) != 1 {
		t.Fatalf("API status: %v %v", statuses, err)
	}
	if err := custody.ConfirmStart(t.Context(), identity, false, testRunPins(identity.AcceptanceID, identity.RunID)); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	_, _ = acquire("successor")
	predecessor, err := custody.ReadStart(t.Context(), receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := custody.ConfirmStart(t.Context(), predecessor, true, testRunPins(predecessor.AcceptanceID, predecessor.RunID)); err != nil {
		t.Fatal(err)
	}
	current, err := custody.ReadStart(t.Context(), receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.PredecessorEpoch != current.OwnerEpoch {
		t.Fatal("adoption did not retain current authority")
	}
}
