//go:build cloudintegration

package podauth

import (
	"context"
	"errors"
	"fmt"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	shared "github.com/goobers/goobers/internal/podauth"
)

func TestGrantRevocationVisibleAcrossReplicas(t *testing.T) {
	pool := revocationPool(t)
	manager, owner := revocationOwner(t, pool, "first")
	var live atomic.Pointer[lease.Handle]
	live.Store(owner)
	writer := &Revocations{Pool: pool, InstanceID: "instance", Lease: manager, LiveOwner: live.Load}
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
	start := time.Now()
	if err := writer.Revoke(t.Context(), grant.ID, grant.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	for _, replica := range []*Auth{a, b} {
		if _, err := replica.VerifyCredentialGrant(t.Context(), token); !errors.Is(err, shared.ErrInvalidCredentialGrant) {
			t.Fatalf("revocation not visible: %v", err)
		}
	}
	if time.Since(start) > RevocationReadTimeout {
		t.Fatal("healthy replicas exceeded revocation bound")
	}
	if err := writer.Revoke(t.Context(), grant.ID, grant.ExpiresAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	var expires time.Time
	if err := pool.QueryRow(t.Context(), `SELECT owner_epoch,expires_at FROM control.grant_revocation WHERE instance_id=$1 AND grant_id=$2`, "instance", grant.ID).Scan(&epoch, &expires); err != nil {
		t.Fatal(err)
	}
	if epoch != owner.Epoch || !expires.Equal(grant.ExpiresAt) {
		t.Fatalf("revocation changed: epoch=%d expiry=%v", epoch, expires)
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

func TestGrantRevocationFencesStaleOwnerAndUsesLiveEpoch(t *testing.T) {
	pool := revocationPool(t)
	first, stale := revocationOwner(t, pool, "first")
	var live atomic.Pointer[lease.Handle]
	live.Store(stale)
	store := &Revocations{Pool: pool, InstanceID: "instance", Lease: first, LiveOwner: live.Load}
	if err := first.Release(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	_, current := revocationOwner(t, pool, "second")
	if err := store.Revoke(t.Context(), "stale-grant", time.Now().Add(time.Hour)); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("stale write = %v", err)
	}
	if revoked, err := store.IsRevoked(t.Context(), "stale-grant"); err != nil || revoked {
		t.Fatalf("stale write committed: %v %v", revoked, err)
	}
	// The same store reads the live handle instead of retaining its construction epoch.
	live.Store(current)
	if err := store.Revoke(t.Context(), "new-grant", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	if err := pool.QueryRow(t.Context(), `SELECT owner_epoch FROM control.grant_revocation WHERE instance_id=$1 AND grant_id=$2`, "instance", "new-grant").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != current.Epoch || epoch <= stale.Epoch {
		t.Fatalf("stored owner epoch=%d current=%d stale=%d", epoch, current.Epoch, stale.Epoch)
	}
	live.Store(nil)
	if err := store.Revoke(t.Context(), "unowned", time.Now().Add(time.Hour)); !errors.Is(err, lease.ErrStale) {
		t.Fatal(err)
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
	start := time.Now()
	_, err = store.IsRevoked(t.Context(), "grant")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked revocation read = %v", err)
	}
	if time.Since(start) > RevocationReadTimeout+time.Second {
		t.Fatal("revocation deadline unbounded")
	}
}

func revocationOwner(t *testing.T, pool *pgxpool.Pool, holder string) (*lease.Manager, *lease.Handle) {
	t.Helper()
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "instance", Name: "control", Holder: holder})
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
