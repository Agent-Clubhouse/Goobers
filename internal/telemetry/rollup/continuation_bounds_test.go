package rollup

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestContinuationDoesNotTrustManualTriggerRef(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	if _, err := db.sql.Exec(`INSERT INTO runs (run_id,workflow,workflow_version,gaggle,trigger_kind,trigger_ref,started_at) VALUES ('ordinary','wf',1,'g','manual','source',?)`, formatTime(fixtureStart)); err != nil {
		t.Fatal(err)
	}
	got, err := db.ContinuationRuns(context.Background(), "source", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != 0 {
		t.Fatalf("ordinary manual run counted as continuation: %+v", got)
	}
}

func TestContinuationPagesUseIndexAtScale(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, t.TempDir())
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := 0; i < 3000; i++ {
		parent := "other"
		if i < 123 {
			parent = "source"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs
  (run_id,workflow,workflow_version,gaggle,trigger_kind,trigger_ref,started_at,continued_from_run_id)
  VALUES (?,'wf',1,'g','manual','source',?,?)`, fmt.Sprintf("child-%04d", i), formatTime(fixtureStart), parent); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	after := ""
	seen := map[string]bool{}
	for {
		page, err := db.ContinuationRuns(ctx, "source", after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) > 50 || page.Incomplete {
			t.Fatalf("invalid page: %+v", page)
		}
		for _, row := range page.Runs {
			if seen[row.ID] {
				t.Fatalf("duplicate %s", row.ID)
			}
			seen[row.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		after = page.NextCursor
	}
	if len(seen) != 123 {
		t.Fatalf("children=%d", len(seen))
	}
	rows, err := db.readDB().QueryContext(ctx, "EXPLAIN QUERY PLAN "+continuationPageQuery, "source", "source", "child-0049", 51)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	indexed := false
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "idx_runs_continuations") {
			indexed = true
		}
		if strings.Contains(detail, "SCAN ") || strings.Contains(detail, "TEMP B-TREE") {
			t.Fatalf("unbounded plan: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("continuation index unused")
	}
}

func TestContinuationUpgradePreservesUnknownUntilIngestion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "telemetry.db")
	old := openHistoricalTestDB(t, path, 31)
	if _, err := old.sql.Exec(`INSERT INTO runs(run_id,workflow,workflow_version,gaggle,trigger_kind,trigger_ref,started_at) VALUES('child','wf',1,'g','manual','source',?)`, formatTime(fixtureStart)); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	page, err := db.ContinuationRuns(t.Context(), "source", "")
	if err != nil {
		t.Fatal(err)
	}
	if !page.Incomplete || len(page.Runs) != 0 {
		t.Fatalf("invented upgraded lineage: %+v", page)
	}
	run, err := journal.Create(filepath.Join(root, "runs"), journal.RunIdentity{RunID: "child", Workflow: "wf", Gaggle: "g", Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "source"}, ContinuedFromRunID: "source"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestRun(t.Context(), filepath.Join(root, "runs", "child")); err != nil {
		t.Fatal(err)
	}
	page, err = db.ContinuationRuns(t.Context(), "source", "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Incomplete || len(page.Runs) != 1 || page.Runs[0].ID != "child" {
		t.Fatalf("ingestion did not complete lineage: %+v", page)
	}
}
