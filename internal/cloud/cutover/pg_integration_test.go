//go:build cloudintegration

package cutover

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/pgroles"
)

func TestBootstrapRealStepsAgainstPostgres(t *testing.T) {
	url := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	const instance = "cutover-it"
	database, err := pgroles.InstanceDatabase(instance)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	// Provision closes the database it runs on (it revokes PUBLIC CONNECT), so
	// it runs on a scratch maintenance database, never the test server's own.
	const maintenance = "cutover_it_maintenance"
	drop := func() {
		for _, name := range []string{database, maintenance} {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		}
	}
	drop()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+maintenance); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drop(); admin.Close() })

	ownerConfig, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	ownerConfig.ConnConfig.Database = database
	ownerConfig.MaxConns = 4
	// The test server's roles have no passwords, so the owner connection uses
	// the test credentials against the provisioned database; production
	// passes a distinct owner URL.
	connect := Connect{
		Admin: func(ctx context.Context) (*pgx.Conn, error) {
			config, err := pgx.ParseConfig(url)
			if err != nil {
				return nil, err
			}
			config.Database = maintenance
			return pgx.ConnectConfig(ctx, config)
		},
		Owner: func(ctx context.Context) (*pgxpool.Pool, error) { return pgxpool.NewWithConfig(ctx, ownerConfig) },
	}
	// Steps whose backends have not shipped are dropped to model the day they
	// land; every shipped step is the real one.
	all, release := PGSteps(connect, instance, ControllerLeaseName, "test-pitr-mark", "bootstrap-test", nil)
	defer release()
	var shipped []Step
	for _, s := range all {
		if s.Issue == 0 {
			shipped = append(shipped, s)
		}
	}
	if len(shipped) != 3 {
		t.Fatalf("shipped steps = %d, want provisioning, migrations and lease row", len(shipped))
	}
	first, err := Bootstrap(ctx, shipped)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	for _, r := range first[1:] { // roles may already exist on the shared server
		if !r.Changed {
			t.Errorf("first run: step %q reported no change", r.Name)
		}
	}
	second, err := Bootstrap(ctx, shipped)
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	for _, r := range second {
		if r.Changed {
			t.Errorf("second run: step %q changed again", r.Name)
		}
	}
	owner, err := connect.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	present, err := PGInstall{Pool: owner, InstanceID: instance, LeaseName: ControllerLeaseName}.LeaseRowPresent(ctx)
	if err != nil || !present {
		t.Fatalf("controller lease row present = %v, %v", present, err)
	}
}
