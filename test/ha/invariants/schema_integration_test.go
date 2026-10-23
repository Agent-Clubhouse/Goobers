//go:build integration && cloudpg

package invariants

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// cleanData is a history that satisfies every invariant.
var cleanData = fmt.Sprintf(`
INSERT INTO control.lease_history VALUES
  ('i','%[1]s',1,'a','2026-01-01 00:00:00+00','2026-01-01 00:10:00+00','voluntary'),
  ('i','%[1]s',2,'b','2026-01-01 00:10:00+00',NULL,NULL);
INSERT INTO journal.run VALUES
  ('i','r1','g','w','\x00','2026-01-01 00:01:00+00',NULL),
  ('i','r2','g','w','\x00','2026-01-01 00:01:00+00',NULL);
INSERT INTO journal.run_seq (instance_id, run_id, last_seq, owner_epoch) VALUES ('i','r1',2,1),('i','r2',1,1);
INSERT INTO journal.run_event (instance_id, run_id, seq, type, emit_key, at, body) VALUES
  ('i','r1',1,'t','k1','2026-01-01 00:01:00+00','\x00'),
  ('i','r1',2,'t','k2','2026-01-01 00:02:00+00','\x00'),
  ('i','r2',1,'t','k1','2026-01-01 00:02:00+00','\x00');
INSERT INTO journal.instance_seq (instance_id, stream, last_seq, owner_epoch) VALUES ('i','main',2,1);
INSERT INTO journal.instance_event (instance_id, stream, seq, at, type, run_id, emit_key, epoch, body) VALUES
  ('i','main',1,'2026-01-01 00:02:00+00','t','r1','e1',1,'\x00'),
  ('i','main',2,'2026-01-01 00:11:00+00','t','r1','e2',2,'\x00');
INSERT INTO control.claim_history VALUES
  ('i','g','gh','42','r1','w','tok1',1,'2026-01-01 00:01:00+00','2026-01-01 00:01:00+00','2026-01-01 00:05:00+00','2026-01-01 00:05:00+00','{}',NULL),
  ('i','g','gh','42','r2','w','tok2',1,'2026-01-01 00:05:00+00','2026-01-01 00:05:00+00','2026-01-01 00:08:00+00','2026-01-01 00:08:00+00','{}',NULL);
INSERT INTO control.claim VALUES
  ('i','g','gh','42','r3','w','tok3',2,'2026-01-01 00:09:00+00','2026-01-01 00:09:00+00','2026-01-01 00:20:00+00',NULL);
INSERT INTO control.acceptance (instance_id, id, idem_key, actor, fingerprint, payload, state, owner_epoch, run_id, accepted_at) VALUES
  ('i','a1','k1','act','\x00','\x00','dispatched',1,'r1','2026-01-01 00:00:30+00'),
  ('i','a2','k2','act','\x00','\x00','dispatched',1,'r2','2026-01-01 00:00:40+00'),
  ('i','a3','k3','act','\x00','\x00','accepted',NULL,'','2026-01-01 00:00:50+00');
INSERT INTO control.source_receipt VALUES
  ('i','src1','act','fp',ARRAY['a1'],'2026-01-01 00:00:30+00'),
  ('i','src2','act','fp',ARRAY['a2','a3'],'2026-01-01 00:00:40+00');
`, lease.ControllerName)

var createsMarker = regexp.MustCompile(`(?m)^-- creates: (\S+)$`)

// newSchemaDB creates a throwaway database, applies the shipped migrations,
// then applies each testdata/pending fixture whose table the shipped
// migrations do not yet create. Fixtures are copies of unmerged migrations.
func newSchemaDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testdep.RequireEnv(t, "GOOBERS_TEST_POSTGRES_URL")
	ctx := context.Background()
	base := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("chaos_inv_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	config, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4 // the migration runner rejects larger pools
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := migrate.Runner{Direct: pool, Catalog: catalog, BackupRef: "chaos-selftest", InstanceID: "i", Holder: "chaos-selftest"}
	if err := runner.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join("testdata", "pending", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("pending fixtures: %v (%d)", err, len(files))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m := createsMarker.FindSubmatch(data)
		if m == nil {
			t.Fatalf("%s has no '-- creates:' marker", f)
		}
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", string(m[1])).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Logf("shipped migrations now create %s; fixture %s is obsolete", m[1], filepath.Base(f))
			continue
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return pool
}

func TestIntegrationInvariantsAgainstRealSchema(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEST_POSTGRES_URL")
	pool := newSchemaDB(t)
	ctx := context.Background()
	inTx := func(t *testing.T, seed bool, stmts ...string) Report {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if seed {
			stmts = append([]string{cleanData}, stmts...)
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				t.Fatalf("%s: %v", strings.TrimSpace(s), err)
			}
		}
		report, err := Check(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	t.Run("clean history holds", func(t *testing.T) {
		report := inTx(t, true)
		if !report.OK() {
			t.Fatalf("clean history violated: %+v", report.Violations)
		}
		if len(report.Skipped) == 0 {
			t.Fatal("pending invariants not reported as skipped")
		}
	})
	t.Run("empty database is not a pass", func(t *testing.T) {
		report := inTx(t, false)
		var want int
		for _, inv := range All() {
			if inv.Pending == "" {
				want++
			}
		}
		if report.OK() || len(report.Uncovered) != want {
			t.Fatalf("uncovered = %v (ok=%v), want all %d invariants uncovered", report.Uncovered, report.OK(), want)
		}
	})
	for _, inv := range All() {
		if inv.Pending != "" {
			continue
		}
		for i, stmts := range injections[inv.Name] {
			t.Run(fmt.Sprintf("%s/%d", inv.Name, i), func(t *testing.T) {
				report := inTx(t, true, stmts...)
				if len(report.Violations) != 1 || report.Violations[0].Invariant != inv.Name || len(report.Uncovered) != 0 {
					t.Fatalf("report = %+v, want only %s", report, inv.Name)
				}
			})
		}
	}
}

func TestIntegrationQueryErrorIsNotAPass(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEST_POSTGRES_URL")
	pool := newSchemaDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DROP TABLE control.claim_history"); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(ctx, pool); err == nil {
		t.Fatal("Check on a missing table returned nil error")
	}
}
