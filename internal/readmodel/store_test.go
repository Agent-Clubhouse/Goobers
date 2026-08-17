package readmodel

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatalf("open read model: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestOpenSeedsAFreshEpoch pins §4.2's central property: the epoch is minted per
// BUILD and is opaque.
//
// It is load-bearing, not bookkeeping. A rebuilt read.db is a new SQLite file, so
// AUTOINCREMENT restarts at 1 — and without an epoch, a client holding cursor
// 918342 reconnects to a store whose maximum is 100. That cursor is neither below
// the retention floor nor from a different schema version, so no named condition
// fires, the client waits forever, and §8.2's rule discarding lower positions
// makes it permanent.
func TestOpenSeedsAFreshEpoch(t *testing.T) {
	ctx := context.Background()
	first := openTestStore(t)
	a, err := first.State(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if a.Epoch == "" {
		t.Fatal("no epoch was minted")
	}
	if a.SchemaVersion != len(migrations) {
		t.Errorf("schema version = %d, want %d", a.SchemaVersion, len(migrations))
	}

	second := openTestStore(t)
	b, err := second.State(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if a.Epoch == b.Epoch {
		t.Error("two separate stores were given the same epoch; each build must be distinguishable")
	}
}

// TestConcurrentFirstOpenSucceeds is issue #2032's acceptance criterion,
// mirroring internal/telemetry/rollup's own #1128 test of the same shape: N
// goroutines each open their OWN *Store handle against the same fresh path
// (simulating separate processes racing to create read.db, e.g. `goobers up`
// against a `goobers portal` CLI invocation) — none of that should ever
// error, including on SQLITE_BUSY_SNAPSHOT, which migrateWithBusyRetry
// backstops.
func TestConcurrentFirstOpenSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			store, err := Open(path)
			if err != nil {
				errs <- fmt.Errorf("open %d: %w", i, err)
				return
			}
			defer func() { _ = store.Close() }()
			if _, err := store.State(context.Background()); err != nil {
				errs <- fmt.Errorf("state %d: %w", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestReopenPreservesTheEpoch pins the other half, which is easy to get
// backwards: a REOPEN is not a rebuild.
//
// Re-minting on every Open would make every daemon restart look like a new epoch
// to connected clients, forcing a full resnapshot for no reason. §4.2's
// epoch_changed exists for a replaced FILE, not for a reopened one.
func TestReopenPreservesTheEpoch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	before, err := store.State(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	after, err := reopened.State(ctx)
	if err != nil {
		t.Fatalf("state after reopen: %v", err)
	}
	if before.Epoch != after.Epoch {
		t.Errorf("epoch changed on reopen (%s -> %s); a restart would force every client to resnapshot",
			before.Epoch, after.Epoch)
	}
}

// TestOpenRefusesANewerSchema pins that this build will not run against a store
// written by a newer one. Silently proceeding is how a read model starts
// answering subtly wrong questions.
func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.writer.Exec(
		`UPDATE projection_state SET schema_version = ? WHERE id = 1`, len(migrations)+5); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("opened a store whose schema is newer than this build supports")
	} else if !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("error = %v; want a clear newer-schema refusal", err)
	}
}

// TestOrderingIndexesServeTheListShapes pins §5.5's authorization-predicate rule
// at the index level: gaggle leads the scoped orderings, and there is an
// unrestricted fast path.
//
// This is the part that is cheap now and expensive later. Once a principal is
// scoped to a subset of gaggles, the scope must be a predicate INSIDE the
// indexed query — filtering after LIMIT silently omits rows (the diagnosis's
// §5.6 failure) and filtering before LIMIT without an index reintroduces the
// scan. Retrofitting a leading column into an ordering index after clients
// depend on a cursor format is far more disruptive than shipping it now.
func TestOrderingIndexesServeTheListShapes(t *testing.T) {
	store := openTestStore(t)
	seedRuns(t, store, 400)

	cases := []struct {
		name  string
		where string
		want  string
	}{
		{"unrestricted recency", "", "idx_run_recency"},
		{"gaggle scoped", "WHERE gaggle = 'gaggle-000'", "idx_run_gaggle_recency"},
		{"gaggle and workflow", "WHERE gaggle = 'gaggle-000' AND workflow = 'wf-0'", "idx_run_gaggle_workflow_recency"},
		{"phase aggregate", "WHERE phase = 'running'", "idx_run_phase_recency"},
		{"active counts by workflow", "WHERE phase = 'running' GROUP BY gaggle, workflow", "idx_run_phase_workflow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := "SELECT run_id, started_at FROM run " + tc.where
			if !strings.Contains(tc.where, "GROUP BY") {
				query += " ORDER BY started_at DESC, run_id ASC LIMIT 51"
			}
			plan := explain(t, store, query)
			t.Logf("plan: %s", plan)
			if !strings.Contains(plan, tc.want) {
				t.Errorf("plan does not use %s:\n%s", tc.want, plan)
			}
			// A temp B-tree means the index is not providing the ordering, so
			// page N degrades with depth even though page 1 looks fine.
			if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
				t.Errorf("plan materializes a sort despite the ordering index:\n%s", plan)
			}
		})
	}
}

func explain(t *testing.T, store *Store, query string) string {
	t.Helper()
	rows, err := store.reader.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(out, " | ")
}

// seedRuns inserts enough rows that the planner prefers an index over a scan.
// With a handful of rows SQLite may reasonably choose a scan regardless, which
// would make the plan assertions test the row count rather than the index.
func seedRuns(t *testing.T, store *Store, n int) {
	t.Helper()
	tx, err := store.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	phases := []string{"running", "completed", "failed", "escalated", "aborted"}
	for i := 0; i < n; i++ {
		_, err := tx.Exec(`
			INSERT INTO run (run_id, gaggle, workflow, phase, terminal, started_at, last_seq)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("%032x", i),
			fmt.Sprintf("gaggle-%03d", i%3),
			fmt.Sprintf("wf-%d", i%4),
			phases[i%len(phases)],
			boolToInt(phases[i%len(phases)] != "running"),
			base.Add(time.Duration(i)*time.Minute).UTC().Format(timeFormat),
			i,
		)
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writer.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
