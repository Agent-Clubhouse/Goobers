package readmodel

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestContinuationRunsReturnsDirectChildren(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for _, identity := range []journal.RunIdentity{
		{RunID: "source", Gaggle: "g", Workflow: "wf"},
		{RunID: "child-b", Gaggle: "g", Workflow: "wf", Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "source"}, ContinuedFromRunID: "source"},
		{RunID: "child-a", Gaggle: "g", Workflow: "wf", Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "source"}, ContinuedFromRunID: "source"},
		{RunID: "unrelated", Gaggle: "g", Workflow: "wf", Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "source"}, ContinuedFromRunID: "grandparent"},
	} {
		projection := ProjectRun(identity, Projection{}, completedRunEvents())
		if err := store.UpsertRun(ctx, projection); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ContinuationRuns(ctx, "source", "")
	if err != nil {
		t.Fatal(err)
	}
	children := got.Runs
	if len(children) != 2 || children[0].ID != "child-a" || children[1].ID != "child-b" {
		t.Fatalf("continuations = %+v", children)
	}
}

func TestContinuationProjectionUpgradeReplaysExistingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i, migration := range migrations {
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
		if err := seedState(ctx, tx, i+1); err != nil {
			t.Fatalf("seed migration %d: %v", i+1, err)
		}
	}
	identity := journal.RunIdentity{
		RunID:              "continuation",
		Gaggle:             "g",
		Workflow:           "wf",
		Trigger:            journal.Trigger{Kind: journal.TriggerManual, Ref: "source"},
		ContinuedFromRunID: "source",
		RequestedTarget:    "review",
		WorkspaceBranch:    "goobers/wf/source",
		WorkspaceBranchSHA: "abc123",
		Inputs:             []journal.InputRef{{Name: "review.verdict", Source: "source"}},
	}
	projection := ProjectRun(identity, Projection{}, completedRunEvents())
	if _, err := tx.ExecContext(ctx, `INSERT INTO run
		(run_id, gaggle, workflow, trigger_kind, trigger_ref, phase, terminal,
			started_at, last_seq, operator_json, projection_version)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, '{}', 3)`,
		projection.Run.RunID, projection.Run.Gaggle, projection.Run.Workflow,
		projection.Run.TriggerKind, projection.Run.TriggerRef, string(projection.Run.Phase),
		formatTime(projection.Run.StartedAt), projection.Run.LastSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projection_state SET ready = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state, err := store.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A built store keeps its readiness; the old-rules row is instead queued
	// for targeted re-projection at startup (#6895).
	if !state.Ready {
		t.Fatal("projection upgrade withdrew readiness from a built store")
	}
	stale, err := store.staleRunIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stale, []string{identity.RunID}) {
		t.Fatalf("continuation projection upgrade did not request a journal replay: stale=%v", stale)
	}
	if err := store.UpsertRun(ctx, projection); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.GetRun(ctx, identity.RunID)
	if err != nil || !found {
		t.Fatalf("read upgraded continuation: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(got.Operator, projection.Run.Operator) {
		t.Fatalf("upgraded continuation operator facts = %+v, want %+v", got.Operator, projection.Run.Operator)
	}
	children, err := store.ContinuationRuns(ctx, "source", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(children.Runs) != 1 || children.Runs[0].ID != identity.RunID {
		t.Fatalf("upgraded continuation reverse lineage = %+v", children)
	}
}

func TestContinuationPagesUseIndexAtScale(t *testing.T) {
	ctx := t.Context()
	store := openTestStore(t)
	tx, err := store.writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := 0; i < 3000; i++ {
		parent := "other"
		if i < 123 {
			parent = "source"
		}
		// Most rows deliberately share the trigger ref but lack continuation
		// identity; a residual post-LIMIT filter would starve these pages.
		operator := fmt.Sprintf(`{"ContinuedFromRunID":%q}`, parent)
		_, err := tx.ExecContext(ctx, `INSERT INTO run
  (run_id,gaggle,workflow,trigger_kind,trigger_ref,phase,terminal,started_at,last_seq,operator_json,projection_version)
  VALUES (?,'g','wf','manual','source','completed',1,'2026-01-01T00:00:00Z',1,?,4)`, fmt.Sprintf("child-%04d", i), operator)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	after := ""
	seen := map[string]bool{}
	for {
		page, err := store.ContinuationRuns(ctx, "source", after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) > 50 {
			t.Fatalf("unbounded page: %d", len(page.Runs))
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
	rows, err := store.reader.QueryContext(ctx, "EXPLAIN QUERY PLAN "+continuationPageQuery, "source", "child-0049", 51)
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
		if strings.Contains(detail, "idx_run_continuations") {
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

func TestContinuationPagesRemainCorrectWithoutIndex(t *testing.T) {
	ctx := t.Context()
	store := openTestStore(t)
	if _, err := store.writer.ExecContext(ctx, `DROP INDEX idx_run_continuations`); err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := 0; i < 123; i++ {
		id := fmt.Sprintf("child-%04d", i)
		parent := "other"
		if i%2 == 0 {
			parent = "source"
			want = append(want, id)
		}
		identity := journal.RunIdentity{RunID: id, Gaggle: "g", Workflow: "wf",
			Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "source"}, ContinuedFromRunID: parent}
		if err := store.UpsertRun(ctx, ProjectRun(identity, Projection{}, completedRunEvents())); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	cursor := ""
	for {
		page, err := store.ContinuationRuns(ctx, "source", cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) > 50 {
			t.Fatalf("unbounded page: %d", len(page.Runs))
		}
		for _, run := range page.Runs {
			got = append(got, run.ID)
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor <= cursor {
			t.Fatalf("cursor did not advance: %q -> %q", cursor, page.NextCursor)
		}
		cursor = page.NextCursor
	}
	if !slices.Equal(got, want) {
		t.Fatalf("children without index = %v, want %v", got, want)
	}
}

func TestContinuationIndexReopenIsIdempotentAndRepairsMissingIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	for _, step := range []string{"create", "reapply", "recreate"} {
		t.Run(step, func(t *testing.T) {
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			state, err := store.State(t.Context())
			if err != nil || state.SchemaVersion != 23 {
				t.Fatalf("schema version must remain rollback-compatible: %+v, %v", state, err)
			}
			var ddl string
			if err := store.reader.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_schema WHERE name = 'idx_run_continuations'`).Scan(&ddl); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(ddl, "json_extract(operator_json, '$.ContinuedFromRunID') = trigger_ref") {
				t.Fatalf("unexpected index: %s", ddl)
			}
			if step == "reapply" {
				if _, err := store.writer.ExecContext(t.Context(), `DROP INDEX idx_run_continuations`); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
