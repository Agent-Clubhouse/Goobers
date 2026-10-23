//go:build cloudintegration

package podauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	shared "github.com/goobers/goobers/internal/podauth"
)

func TestGrantRevocationVisibleAcrossReplicas(t *testing.T) {
	pool := revocationPool(t)
	writer := &Revocations{Pool: pool, InstanceID: "instance"}
	other, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	reader := &Revocations{Pool: other, InstanceID: "instance"}
	a := authFixture(t, &testRevocations{})
	a.revocations = writer
	b := authFixture(t, &testRevocations{})
	b.revocations = reader
	token, grant, err := a.GrantKeys.MintCredentialGrant(shared.CredentialGrant{RunID: "run", Stage: "stage", Capabilities: []string{"repo:push"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, replica := range []*Auth{a, b} {
		if _, err := replica.VerifyCredentialGrant(t.Context(), token); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Revoke(t.Context(), grant.ID, grant.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	for _, replica := range []*Auth{a, b} {
		if _, err := replica.VerifyCredentialGrant(t.Context(), token); !errors.Is(err, shared.ErrInvalidCredentialGrant) {
			t.Fatalf("revocation not visible: %v", err)
		}
	}
	if err := writer.Revoke(t.Context(), grant.ID, grant.ExpiresAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var epoch *int64
	var expires time.Time
	if err := pool.QueryRow(t.Context(), `SELECT owner_epoch,expires_at FROM control.grant_revocation WHERE instance_id=$1 AND grant_id=$2`, "instance", grant.ID).Scan(&epoch, &expires); err != nil {
		t.Fatal(err)
	}
	if epoch != nil || !expires.Equal(grant.ExpiresAt) {
		t.Fatalf("revocation changed: epoch=%v expiry=%v", epoch, expires)
	}
	isolated := &Revocations{Pool: other, InstanceID: "other-instance"}
	if revoked, err := isolated.IsRevoked(t.Context(), grant.ID); err != nil || revoked {
		t.Fatalf("cross-instance revocation: %v %v", revoked, err)
	}
	other.Close()
	if _, err := b.VerifyCredentialGrant(t.Context(), token); err == nil {
		t.Fatal("closed database admitted grant")
	}
}

func TestGrantRevocationDoesNotRequireControllerAndCleanupFences(t *testing.T) {
	pool := revocationPool(t)
	store := &Revocations{Pool: pool, InstanceID: "instance"}
	if err := store.Revoke(t.Context(), "expired", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(t.Context(), "live", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	first, stale := revocationOwner(t, pool, "first")
	if err := first.Release(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	second, current := revocationOwner(t, pool, "second")
	if err := PruneExpired(t.Context(), first, stale); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale prune: %v", err)
	}
	if revoked, err := store.IsRevoked(t.Context(), "expired"); err != nil || !revoked {
		t.Fatalf("stale cleanup changed fact: %v %v", revoked, err)
	}
	if err := store.Revoke(t.Context(), "after-takeover", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := PruneExpired(t.Context(), second, current); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"live", "after-takeover"} {
		if revoked, err := store.IsRevoked(t.Context(), id); err != nil || !revoked {
			t.Fatalf("unexpired %s removed: %v %v", id, revoked, err)
		}
	}
	if revoked, err := store.IsRevoked(t.Context(), "expired"); err != nil || revoked {
		t.Fatalf("expired fact retained: %v %v", revoked, err)
	}
}

func TestGrantRevocationReadDeadlineFailsClosed(t *testing.T) {
	pool := revocationPool(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `LOCK TABLE control.grant_revocation IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	store := &Revocations{Pool: pool, InstanceID: "instance"}
	err = store.CheckGrant(t.Context(), "grant", time.Now().Add(time.Hour))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked revocation read = %v", err)
	}
}

func revocationOwner(t *testing.T, pool *pgxpool.Pool, holder string) (*lease.Manager, *lease.Handle) {
	t.Helper()
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "instance", Name: lease.ControllerName, Holder: holder})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return manager, owner
}

func revocationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("grant_revocation_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	config.MaxConns = lease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	pgtest.EnsureRoles(t, pool)
	if err := (migrate.Runner{Direct: pool, Catalog: catalog, BackupRef: "local-test", InstanceID: "migration", Holder: "test"}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool
}
