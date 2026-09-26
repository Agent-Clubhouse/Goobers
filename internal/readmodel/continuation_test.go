package readmodel

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
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
	} {
		projection := ProjectRun(identity, Projection{}, completedRunEvents())
		if err := store.UpsertRun(ctx, projection); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ContinuationRuns(ctx, []string{"source"})
	if err != nil {
		t.Fatal(err)
	}
	children := got["source"]
	if len(children) != 2 || children[0].RunID != "child-a" || children[1].RunID != "child-b" {
		t.Fatalf("continuations = %+v", children)
	}
}

func TestContinuationLineageUpgradeReplaysExistingRows(t *testing.T) {
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
	for i, migration := range migrations[:len(migrations)-1] {
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
	if state.Ready {
		t.Fatal("continuation lineage migration did not request a journal replay")
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
	children, err := store.ContinuationRuns(ctx, []string{"source"})
	if err != nil {
		t.Fatal(err)
	}
	if len(children["source"]) != 1 || children["source"][0].RunID != identity.RunID {
		t.Fatalf("upgraded continuation reverse lineage = %+v", children)
	}
}
