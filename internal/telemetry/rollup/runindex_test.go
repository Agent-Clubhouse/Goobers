package rollup

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunListIndexesAreUsedByThePlanner pins that the v17 indexes actually serve
// RunRefPage's query shape.
//
// An index that exists but is not chosen is worse than no index: it costs write
// amplification and disk while the read stays a full scan, and nothing fails. The
// design's §5.2 names exactly this — "today the 'indexed' list path is a full
// scan plus sort" — so the assertion is on the PLAN, not on latency.
//
// Note what this does and does not prove. It proves the ordering index is used
// and no temporary sort is materialized. It does NOT prove the absence of a
// residual predicate: §5.7 establishes that EXPLAIN QUERY PLAN does not enumerate
// residual terms, which is why the real bound comes from an enumerated set of
// supported filter combinations in Wave 2 rather than from a plan property.
func TestRunListIndexesAreUsedByThePlanner(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatalf("open rollup: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedRunsForIndexPlan(t, db)

	cases := []struct {
		name      string
		filter    RunListFilter
		wantIndex string
	}{
		{
			name:      "unrestricted recency",
			filter:    RunListFilter{},
			wantIndex: "idx_runs_recency",
		},
		{
			name:      "gaggle scoped",
			filter:    RunListFilter{Gaggle: "alpha"},
			wantIndex: "idx_runs_gaggle_recency",
		},
		{
			name:      "gaggle and workflow scoped",
			filter:    RunListFilter{Gaggle: "alpha", Workflow: "deploy"},
			wantIndex: "idx_runs_gaggle_workflow_recency",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainRunRefPage(t, db, tc.filter)
			t.Logf("plan: %s", plan)
			if !strings.Contains(plan, tc.wantIndex) {
				t.Errorf("plan does not use %s:\n%s", tc.wantIndex, plan)
			}
			// A temp B-tree for ORDER BY means the index is not providing the
			// ordering, so page N degrades with depth even though page 1 looks fine.
			if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
				t.Errorf("plan materializes a sort despite the ordering index:\n%s", plan)
			}
			// A bare "SCAN runs" with no index is the full table scan §5.2
			// describes. "SCAN runs USING COVERING INDEX ..." is not that: it is
			// an ordered walk of the index, which is the optimal plan for an
			// unrestricted recency query with no equality predicate — it reads in
			// order and stops at LIMIT.
			if strings.Contains(plan, "SCAN runs") && !strings.Contains(plan, "USING") {
				t.Errorf("plan performs a full table scan:\n%s", plan)
			}
			// Covering is what makes the page cheap: every column the statement
			// selects is in the index, so there is no per-row table lookup.
			if !strings.Contains(plan, "COVERING INDEX") {
				t.Logf("note: index is used but not covering; each row costs a table lookup:\n%s", plan)
			}
		})
	}
}

// explainRunRefPage returns the query plan for the filter, built by running the
// same statement RunRefPage constructs.
func explainRunRefPage(t *testing.T, db *DB, filter RunListFilter) string {
	t.Helper()
	query, args, empty := runRefPageQuery(filter, time.Time{}, "", 51)
	if empty {
		t.Fatal("filter matched nothing; the plan assertion needs a real query")
	}
	rows, err := db.sql.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(lines, " | ")
}

// seedRunsForIndexPlan inserts enough rows that the planner prefers an index
// over a scan. With a handful of rows SQLite may reasonably choose a scan
// regardless, which would make the assertion test the row count rather than the
// index.
func seedRunsForIndexPlan(t *testing.T, db *DB) {
	t.Helper()
	gaggles := []string{"alpha", "beta"}
	workflows := []string{"deploy", "review"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tx, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		_, err := tx.Exec(
			`INSERT INTO runs (run_id, workflow, workflow_version, gaggle, status, started_at)
			 VALUES (?, ?, 1, ?, 'completed', ?)`,
			runIDForIndex(i), workflows[i%len(workflows)], gaggles[i%len(gaggles)],
			formatTime(base.Add(time.Duration(i)*time.Minute)).String,
		)
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// runIDForIndex returns a unique 32-hex-character run id, matching the shape
// real run ids take.
func runIDForIndex(i int) string {
	return fmt.Sprintf("%032x", i)
}
