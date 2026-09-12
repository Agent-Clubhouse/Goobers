package readmodel

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestReviewEvidenceUpgradeRefreshesSamePositionOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// Build the actual pre-feature schema; do not simulate a fresh v19 store.
	for i, migration := range migrations[:18] {
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			t.Fatal(err)
		}
		if err := seedState(ctx, tx, i+1); err != nil {
			t.Fatal(err)
		}
	}
	p := ProjectRun(testIdentity(), Projection{}, completedRunEvents())
	if _, err := tx.ExecContext(ctx, `INSERT INTO run
		(run_id, gaggle, workflow, phase, terminal, started_at, last_seq, operator_json)
		VALUES (?, ?, ?, ?, 1, ?, ?, '{}')`, p.Run.RunID, p.Run.Gaggle, p.Run.Workflow,
		string(p.Run.Phase), formatTime(p.Run.StartedAt), p.Run.LastSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projection_state SET ready = 1, projection_floor = '2020-01-01T00:00:00.000000000Z' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO monitor_nomination(marker, claimed_at) VALUES ('retained-claim', '2026-01-01T00:00:00Z')`); err != nil {
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
		t.Fatal("upgrade did not request a journal rebuild")
	}
	if _, found, err := store.GetRun(ctx, p.Run.RunID); err != nil || !found {
		t.Fatalf("upgrade discarded old run: found=%v err=%v", found, err)
	}
	var floor, claim string
	if err := store.writer.QueryRowContext(ctx, `SELECT projection_floor FROM projection_state WHERE id = 1`).Scan(&floor); err != nil {
		t.Fatal(err)
	}
	if err := store.writer.QueryRowContext(ctx, `SELECT marker FROM monitor_nomination`).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	if floor != "2020-01-01T00:00:00.000000000Z" || claim != "retained-claim" {
		t.Fatalf("upgrade lost policy state: floor=%q claim=%q", floor, claim)
	}

	p.Run.Operator.ReviewVerdict = string(apiv1.VerdictDefer)
	p.Run.Operator.ReviewReasonCode = apiv1.VerdictReasonNoLander
	p.Run.Operator.ReviewRationale = "  Original rationale.\n\nPreserve every paragraph.  "
	p.Run.Operator.ReviewFindings = []apiv1.Finding{{Severity: apiv1.SeverityInfo, Message: "Original finding."}}
	if err := store.UpsertRun(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.GetRun(ctx, p.Run.RunID)
	if err != nil || !found {
		t.Fatalf("read upgraded run: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(got.Operator, p.Run.Operator) {
		t.Fatalf("same-position upgrade lost evidence: got=%+v want=%+v", got.Operator, p.Run.Operator)
	}
	if got.Disposition != DispositionProduced {
		t.Fatalf("same-position upgrade disposition = %q, want %q; historical terminal unknown row was not repaired", got.Disposition, DispositionProduced)
	}
	before, err := store.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRun(ctx, p); err != nil {
		t.Fatal(err)
	}
	after, err := store.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("unchanged replay published another change: %d -> %d", before, after)
	}
}
