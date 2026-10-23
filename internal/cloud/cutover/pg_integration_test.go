//go:build cloudintegration

package cutover

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBootstrapRealStepsAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	// The steps whose backends have not shipped are dropped to model the day
	// they land; every shipped step is the real one.
	var shipped []Step
	for _, s := range PGSteps(pool, "bootstrap-instance", ControllerLeaseName, "test-pitr-mark", "bootstrap-test") {
		if s.Issue == 0 {
			shipped = append(shipped, s)
		}
	}
	first, err := Bootstrap(ctx, shipped)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	for _, r := range first {
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
	present, err := PGInstall{Pool: pool, InstanceID: "bootstrap-instance", LeaseName: ControllerLeaseName}.LeaseRowPresent(ctx)
	if err != nil || !present {
		t.Fatalf("controller lease row present = %v, %v", present, err)
	}
}

// testPool returns a pool on a database created for this test alone, so the
// shared cloud-pg database other packages use is never touched.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	name := "cutover_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+ident); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
		admin.Close()
	})
	return pool
}
