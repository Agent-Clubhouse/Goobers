//go:build cloudintegration

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudcontrol "github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/podauth"
	shared "github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestControlDatabaseRolesAreSeparated(t *testing.T) {
	instance, apiURL, controllerURL := controlRoleDatabase(t)
	t.Setenv("GOOBERS_INSTANCE_ID", "wrong-env-instance")
	// Production requires a release version; exercise the actual schema checker
	// with the current release rather than the test binary's development stamp.
	priorCheck := checkCloudSchema
	checkCloudSchema = func(ctx context.Context, pool *pgxpool.Pool, _ string) error { return priorCheck(ctx, pool, "0.7.0") }
	t.Cleanup(func() { checkCloudSchema = priorCheck })
	prior := runControlWithDatabases
	called := false
	runControlWithDatabases = func(ctx context.Context, db controlDatabases, _ controlOptions, _ *log.Logger) error {
		called = true
		if db.InstanceID != instance {
			t.Fatalf("instance source drift: %s", db.InstanceID)
		}
		// The real entry path binds the object store to the controller role only.
		if db.Controller.Blobs == nil || db.Controller.Objects == nil || db.API.Blobs != nil || db.API.Objects != nil {
			t.Fatalf("object store binding: controller blobs=%v objects=%v api blobs=%v", db.Controller.Blobs != nil, db.Controller.Objects != nil, db.API.Blobs != nil)
		}
		for role, pool := range map[string]*pgxpool.Pool{"goobers_api": db.API.Pools.Transaction, "goobers_controller": db.Controller.Pools.Transaction} {
			var user string
			if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&user); err != nil {
				return err
			}
			if user != role {
				t.Fatalf("%s path authenticated as %s", role, user)
			}
		}
		record, _, err := db.API.Acceptances.Accept(ctx, "api-key", "actor", []byte("payload"), time.Now())
		if err != nil {
			return err
		}
		if _, err := db.API.Pools.Transaction.Exec(ctx, "DELETE FROM control.acceptance WHERE id=$1", record.ID); err == nil {
			t.Fatal("API obtained acceptance DELETE")
		}
		if _, err := db.API.Pools.Transaction.Exec(ctx, "UPDATE control.acceptance SET state='dispatching' WHERE id=$1", record.ID); err == nil {
			t.Fatal("API obtained dispatch UPDATE")
		}
		if _, err := db.API.Pools.Transaction.Exec(ctx, "SELECT * FROM control.lease"); err == nil {
			t.Fatal("API obtained controller lease authority")
		}
		if _, err := db.API.StartStatusPage(ctx, "", 100); err != nil {
			return err
		}
		revocations := &podauth.Revocations{Pool: db.API.Pools.Transaction, InstanceID: instance}
		if err := revocations.Revoke(ctx, "api-grant", time.Now().Add(time.Hour)); err != nil {
			return err
		}
		if revoked, err := revocations.IsRevoked(ctx, "api-grant"); err != nil || !revoked {
			t.Fatalf("API revocation failed: %v %v", revoked, err)
		}
		if _, err := db.Controller.Pools.Transaction.Exec(ctx, "INSERT INTO control.grant_revocation(instance_id,grant_id,expires_at) VALUES($1,'forbidden',clock_timestamp())", instance); err == nil {
			t.Fatal("controller obtained API append authority")
		}
		return nil
	}
	t.Cleanup(func() { runControlWithDatabases = prior })
	args := []string{"control", "--object-store-url", "https://store.invalid", "--instance-id", instance, "--api-database-url", apiURL, "--api-database-direct-url", apiURL, "--controller-database-url", controllerURL, "--controller-database-direct-url", controllerURL, "--config-dir", t.TempDir()}
	code, _, stderr := runForTest(args...)
	if code != 0 || !called {
		t.Fatalf("code=%d called=%v error=%s", code, called, stderr)
	}
	// Authenticating both surfaces with controller credentials must fail before assembly.
	called = false
	args[6] = controllerURL
	code, _, stderr = runForTest(args...)
	if code != 1 || called || !strings.Contains(stderr, "API database") {
		t.Fatalf("cross-role connection accepted: code=%d called=%v error=%s", code, called, stderr)
	}
}

func TestControlAPIRevocationUnderSetRoleNeedsNoLeasePrivileges(t *testing.T) {
	instance, apiURL, _ := controlRoleDatabase(t)
	apiConfig, err := pgxpool.ParseConfig(apiURL)
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(os.Getenv("GOOBERS_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = apiConfig.ConnConfig.Database
	config.MaxConns = 1
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_api")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var role string
	var canSelectLease, canUpdateLease bool
	if err := pool.QueryRow(t.Context(), `SELECT current_user,
 has_table_privilege(current_user,'control.lease','SELECT'),
 has_table_privilege(current_user,'control.lease','UPDATE')`).Scan(&role, &canSelectLease, &canUpdateLease); err != nil {
		t.Fatal(err)
	}
	if role != "goobers_api" || canSelectLease || canUpdateLease {
		t.Fatalf("API role has lease authority: role=%s select=%v update=%v", role, canSelectLease, canUpdateLease)
	}
	// Execute the real append and retry under SET ROLE. Any lease query or
	// row-lock join in Revoke would fail with these privileges.
	revocations := &podauth.Revocations{Pool: pool, InstanceID: instance}
	for range 2 {
		if err := revocations.Revoke(t.Context(), "set-role-grant", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if revoked, err := revocations.IsRevoked(t.Context(), "set-role-grant"); err != nil || !revoked {
		t.Fatalf("API revocation visibility: revoked=%v err=%v", revoked, err)
	}
	for _, check := range []struct {
		id     string
		expiry time.Time
		want   error
	}{
		{"set-role-grant", time.Now().Add(time.Hour), shared.ErrInvalidCredentialGrant},
		{"unrevoked", time.Now().Add(-time.Hour), shared.ErrExpiredCredentialGrant},
		{"unrevoked", time.Now().Add(time.Hour), nil},
	} {
		if err := revocations.CheckGrant(t.Context(), check.id, check.expiry); !errors.Is(err, check.want) {
			t.Fatalf("API grant admission %s: %v, want %v", check.id, err, check.want)
		}
	}

}

func TestControlRevocationCleanupNeedsOnlySelectDelete(t *testing.T) {
	instance, apiURL, controllerURL := controlRoleDatabase(t)
	api, err := pgxpool.New(t.Context(), apiURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	revocations := &podauth.Revocations{Pool: api, InstanceID: instance}
	if err := revocations.Revoke(t.Context(), "expired", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(controllerURL)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = lease.MaxFencedConnections
	controller, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	var canInsert, canUpdate bool
	if err := controller.QueryRow(t.Context(), `SELECT
 has_table_privilege(current_user,'control.grant_revocation','INSERT'),
 has_any_column_privilege(current_user,'control.grant_revocation','UPDATE')`).Scan(&canInsert, &canUpdate); err != nil {
		t.Fatal(err)
	}
	if canInsert || canUpdate {
		t.Fatalf("controller can mutate revocation facts: insert=%v update=%v", canInsert, canUpdate)
	}
	manager, err := lease.New(lease.Config{Pool: controller, InstanceID: instance, Name: lease.ControllerName, Holder: "pruner"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := podauth.PruneExpired(t.Context(), manager, handle); err != nil {
		t.Fatal(err)
	}
	if revoked, err := revocations.IsRevoked(t.Context(), "expired"); err != nil || revoked {
		t.Fatalf("expired revocation cleanup: revoked=%v err=%v", revoked, err)
	}
}

func controlRoleDatabase(t *testing.T) (instance, apiURL, controllerURL string) {
	t.Helper()
	source := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if source == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	instance = fmt.Sprintf("control_%d", time.Now().UnixNano())
	database, err := controlInstanceDatabase(instance)
	if err != nil {
		t.Fatal(err)
	}
	identifier := pgx.Identifier{database}.Sanitize()
	pgtest.EnsureRoles(t, admin)
	for _, role := range []string{"goobers_api", "goobers_controller"} {
		if _, err := admin.Exec(t.Context(), "ALTER ROLE "+role+" PASSWORD 'role-test'"); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = database
	config.MaxConns = 4
	owner, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		owner.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := (migrate.Runner{Direct: owner, Catalog: catalog, BackupRef: "local-test", InstanceID: instance, Holder: "test"}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	parsed.User = url.UserPassword("goobers_api", "role-test")
	apiURL = parsed.String()
	parsed.User = url.UserPassword("goobers_controller", "role-test")
	controllerURL = parsed.String()
	return instance, apiURL, controllerURL
}

func TestControlInventoryRevokesLegacyPrivileges(t *testing.T) {
	_, apiURL, _ := controlRoleDatabase(t)
	apiConfig, err := pgxpool.ParseConfig(apiURL)
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(os.Getenv("GOOBERS_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = apiConfig.ConnConfig.Database
	config.MaxConns = lease.MaxFencedConnections
	owner, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// Simulate grants from the previous binary, then run the real migration
	// reconcile without introducing a privilege-changing migration.
	for _, sql := range []string{
		"GRANT DELETE ON control.acceptance TO goobers_api",
		"GRANT INSERT(instance_id,grant_id,owner_epoch,expires_at) ON control.grant_revocation TO goobers_controller",
	} {
		if _, err := owner.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := (migrate.Runner{Direct: owner, Catalog: catalog, BackupRef: "local-test", InstanceID: "migration", Holder: "test"}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	var apiDelete, controllerInsert bool
	if err := owner.QueryRow(t.Context(), `SELECT
 has_table_privilege('goobers_api','control.acceptance','DELETE'),
 has_any_column_privilege('goobers_controller','control.grant_revocation','INSERT')`).Scan(&apiDelete, &controllerInsert); err != nil {
		t.Fatal(err)
	}
	if apiDelete || controllerInsert {
		t.Fatalf("legacy grants survived reconcile: api delete=%v controller insert=%v", apiDelete, controllerInsert)
	}
}

func TestControlControllerInventoryCoversDispatchSnapshotsAndPrune(t *testing.T) {
	instance, apiURL, controllerURL := controlRoleDatabase(t)
	api, err := pgxpool.New(t.Context(), apiURL)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	controllerConfig, err := pgxpool.ParseConfig(controllerURL)
	if err != nil {
		t.Fatal(err)
	}
	controllerConfig.MaxConns = lease.MaxFencedConnections
	controller, err := pgxpool.NewWithConfig(t.Context(), controllerConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	manager, err := lease.New(lease.Config{Pool: controller, InstanceID: instance, Name: lease.ControllerName, Holder: "dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apiStore, err := acceptance.New(api, instance)
	if err != nil {
		t.Fatal(err)
	}
	store, err := acceptance.New(controller, instance)
	if err != nil {
		t.Fatal(err)
	}
	store.BindFence(manager, handle)
	record, _, err := apiStore.Accept(t.Context(), "expired", "operator", []byte("payload"), time.Now().Add(-triggerqueue.ReplayRetention-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	authority, err := store.ReadStartAuthority(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStart(t.Context(), record.ID, authority.RunID); err != nil {
		t.Fatal(err)
	}
	observations := cloudcontrol.Observations{Pool: controller, InstanceID: instance, Lease: manager, Handle: handle}
	if err := observations.Replace(t.Context(), []cloudcontrol.Observation{{Key: "quota", Payload: []byte(`{"remaining":1}`), ObservedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if got, err := observations.Load(t.Context()); err != nil || len(got) != 1 {
		t.Fatalf("snapshot read: %v %v", got, err)
	}
	if err := store.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, repeated, err := apiStore.Accept(t.Context(), "expired", "operator", []byte("payload"), time.Now())
	if err != nil || repeated {
		t.Fatalf("controller did not prune receipt: repeated=%v err=%v", repeated, err)
	}
}
