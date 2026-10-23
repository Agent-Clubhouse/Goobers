package invariants

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Test schema: the columns the invariant queries read, in the portable subset.
const testSchema = `
ATTACH DATABASE ':memory:' AS control;
ATTACH DATABASE ':memory:' AS journal;
CREATE TABLE journal.event (instance_id TEXT, run_id TEXT, branch TEXT, seq INTEGER, emit_key TEXT, owner_epoch INTEGER, committed_at INTEGER);
CREATE TABLE control.claim_history (instance_id TEXT, gaggle TEXT, provider TEXT, external_id TEXT, run_id TEXT, held_from INTEGER, held_until INTEGER);
CREATE TABLE control.acceptance (instance_id TEXT, source_kind TEXT, source_key TEXT);
CREATE TABLE control.lease_history (instance_id TEXT, name TEXT, epoch INTEGER, holder TEXT, acquired_at INTEGER, released_at INTEGER);
CREATE TABLE control.start_audit (instance_id TEXT, acceptance_id TEXT, epoch INTEGER, outcome TEXT, claim_written INTEGER, slot_written INTEGER, journal_written INTEGER);
CREATE TABLE control.stage_exec_audit (instance_id TEXT, run_id TEXT, stage TEXT, pod_role TEXT);
`

// cleanData is a history that satisfies every invariant.
const cleanData = `
INSERT INTO journal.event VALUES ('i','r1','main',1,'k1',1,10),('i','r1','main',2,'k2',1,11),('i','r1','b2',1,'k3',2,30);
INSERT INTO control.claim_history VALUES ('i','g','gh','42','r1',0,10),('i','g','gh','42','r2',10,NULL);
INSERT INTO control.acceptance VALUES ('i','webhook','d1'),('i','cron','t1'),('i','cron','t2');
INSERT INTO control.lease_history VALUES ('i','control',1,'a',0,20),('i','control',2,'b',20,NULL);
INSERT INTO control.start_audit VALUES ('i','acc1',1,'not_mine',0,0,0),('i','acc2',2,'started',1,1,1);
INSERT INTO control.stage_exec_audit VALUES ('i','r1','build','worker');
`

// injections maps each invariant to a statement that violates only it.
var injections = map[string]string{
	"no-duplicate-emit-key":              `INSERT INTO journal.event VALUES ('i','r1','extra',1,'k1',2,31)`,
	"journal-seq-contiguous":             `INSERT INTO journal.event VALUES ('i','r9','main',1,'a',2,40),('i','r9','main',3,'b',2,41)`,
	"no-claim-with-two-runs":             `INSERT INTO control.claim_history VALUES ('i','g','gh','42','r3',5,NULL)`,
	"one-acceptance-per-source-key":      `INSERT INTO control.acceptance VALUES ('i','cron','t1')`,
	"no-overlapping-lease-intervals":     `INSERT INTO control.lease_history VALUES ('i','control',3,'c',35,NULL)`,
	"no-notmine-side-effects":            `INSERT INTO control.start_audit VALUES ('i','acc3',1,'not_mine',0,1,0)`,
	"no-stale-owner-emits":               `INSERT INTO journal.event VALUES ('i','r8','main',1,'z',1,25)`,
	"no-stage-executed-by-control-plane": `INSERT INTO control.stage_exec_audit VALUES ('i','r1','deploy','control')`,
}

func openFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // ATTACH is per connection
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{testSchema, cleanData} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestCleanHistoryHoldsEveryInvariant(t *testing.T) {
	t.Parallel()
	report, err := Check(context.Background(), openFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("clean history violated: %+v", report.Violations)
	}
	if len(report.Checked) != len(All()) {
		t.Fatalf("checked %d of %d invariants", len(report.Checked), len(All()))
	}
}

func TestSelfTestDetectsEachInjectedViolation(t *testing.T) {
	t.Parallel()
	for _, inv := range All() {
		stmt, ok := injections[inv.Name]
		if !ok {
			t.Fatalf("invariant %s has no injected-violation self-test", inv.Name)
		}
		t.Run(inv.Name, func(t *testing.T) {
			t.Parallel()
			db := openFixture(t)
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
			report, err := Check(context.Background(), db)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Violations) != 1 || report.Violations[0].Invariant != inv.Name {
				t.Fatalf("violations = %+v, want only %s", report.Violations, inv.Name)
			}
		})
	}
	if len(injections) != len(All()) {
		t.Fatalf("%d injections for %d invariants", len(injections), len(All()))
	}
}

func TestQueryErrorIsNotAPass(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := Check(context.Background(), db); err == nil {
		t.Fatal("Check on an empty schema returned nil error")
	}
}

func TestWriteEvidence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := (Report{Checked: []string{"x"}}).WriteEvidence(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "invariants.json")); err != nil {
		t.Fatal(err)
	}
}
