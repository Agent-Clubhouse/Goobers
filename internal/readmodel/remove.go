package readmodel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

// Removal and the restart query (#1923, design §6.1).

// RemoveRun deletes a run's projected rows and publishes run.removed.
//
// One transaction, like every other projected write, and for the same reason:
// a client that saw the change row must find the row already gone, not
// racing its deletion.
//
// The change row is emitted even when no run row existed. A removal the store
// had already applied is still worth publishing if a marker asked for it —
// the alternative is that a client which missed the first removal never learns
// of it, and silence is indistinguishable from "still there".
func (s *Store) RemoveRun(ctx context.Context, runID string) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("readmodel: begin remove: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read identity before deleting, so the change row can carry the gaggle and
	// workflow a scoped client filters on. After the delete they are gone, and a
	// change with no scope would either be broadcast to every client or dropped
	// by all of them.
	var gaggle, workflow string
	err = tx.QueryRowContext(ctx,
		`SELECT gaggle, workflow FROM run WHERE run_id = ?`, runID).Scan(&gaggle, &workflow)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("readmodel: read %s before removal: %w", runID, err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM run_stage WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("readmodel: delete stages for %s: %w", runID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM run_node WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("readmodel: delete nodes for %s: %w", runID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM remediation_example WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("readmodel: delete remediation examples for %s: %w", runID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM run WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("readmodel: delete run %s: %w", runID, err)
	}
	// The removal's timestamp is wall-clock, unlike every other change row.
	//
	// Everywhere else the projection is a pure function of the journal and the
	// change carries last_activity_at, so two rebuilds of the same journals
	// produce identical feeds (§14.9). A removal has no journal to read a time
	// from — that is precisely what happened — so there is nothing else to use.
	// It does not break §14.9 because a rebuild does not reproduce removals from
	// journals at all; the projection floor and tombstones are copied forward
	// (§6.5), which is why they exist as policy state rather than derived facts.
	if err := appendChange(ctx, tx, s.now(), ChangeRunRemoved, RunRow{
		RunID: runID, Gaggle: gaggle, Workflow: workflow,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("readmodel: commit remove: %w", err)
	}
	return nil
}

// NonTerminalRuns returns runs the read model records as still in flight.
//
// This is the restart pass's second half (§6.1). Only two categories of run can
// have changed while the projector was down: one a writer reported (a pending
// intake marker) and one that was still running when we stopped. A terminal run
// cannot advance — that is what terminal means — so re-reading it would be work
// with a known-empty result.
//
// On the live instance this is tens of rows against 40,665 directories. It is
// served by idx_run_phase_recency, so the cost is proportional to the answer
// rather than to history.
func (s *Store) NonTerminalRuns(ctx context.Context, limit int) ([]RunRow, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, `
		SELECT `+runColumns+`
		FROM run r
		WHERE r.terminal = 0
		ORDER BY r.started_at DESC, r.run_id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("readmodel: read non-terminal runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunRow
	for rows.Next() {
		row, err := scanRunRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: non-terminal rows: %w", err)
	}
	return out, nil
}

// PhaseOf reports a projected run's phase, for callers that need only that.
func (s *Store) PhaseOf(ctx context.Context, runID string) (journal.RunPhase, bool, error) {
	var phase string
	db, release, err := s.readHandle()
	if err != nil {
		return "", false, err
	}
	defer release()
	err = db.QueryRowContext(ctx,
		`SELECT phase FROM run WHERE run_id = ?`, runID).Scan(&phase)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("readmodel: read phase of %s: %w", runID, err)
	}
	return journal.RunPhase(phase), true, nil
}
