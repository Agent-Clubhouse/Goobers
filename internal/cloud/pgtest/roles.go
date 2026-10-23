//go:build cloudintegration

// Package pgtest holds fixtures for cloud PostgreSQL integration tests. It is
// compiled only with the cloudintegration tag.
package pgtest

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/pgroles"
)

// EnsureRoles creates or converges the cluster-wide roles (including the
// runtime roles migrate requires) exactly as goobers-cloud bootstrap does,
// through pgroles. Roles are shared by every test on the server, so fixtures
// never create them ad hoc and never drop them; tests isolate themselves with
// per-test databases instead. CI runs these packages with -p 1 so no two
// packages converge roles concurrently.
func EnsureRoles(t *testing.T, pool *pgxpool.Pool) {
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
