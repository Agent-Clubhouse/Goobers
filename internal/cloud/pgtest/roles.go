//go:build cloudintegration || cloudpg

// Package pgtest holds fixtures for cloud PostgreSQL integration tests. It is
// compiled only with the cloudintegration or cloudpg tag.
package pgtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/pgroles"
)

// EnsureRoles creates or converges the cluster-wide roles (including the
// runtime roles migrate requires) exactly as goobers-cloud bootstrap does,
// through pgroles. Roles are shared by every test on the server, so fixtures
// never create them ad hoc and never drop them; tests isolate themselves with
// per-test databases instead. CI runs these packages with -p 1 so no two
// packages converge roles concurrently.
func EnsureRoles(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := pgroles.ProvisionRoles(t.Context(), conn.Conn(), nil); err != nil {
		t.Fatalf("provision cloud roles: %v", err)
	}
}

// ControllerPool returns a pool whose every connection runs SET ROLE
// goobers_controller, so a store built on it is exercised under exactly the
// grants the write-path inventory reviewed, not as the superuser fixture.
func ControllerPool(t testing.TB, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := pool.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	controller, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	return controller
}
