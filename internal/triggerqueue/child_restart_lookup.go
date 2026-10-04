package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ActiveRunID selects execution while RunID continues to identify acceptance.
func (c ChildRecord) ActiveRunID() string {
	if c.ExecutionEpoch == 0 {
		return c.RunID
	}
	return c.ExecutionRunID
}

const childExecutionMetadataColumns = `epoch,run_id,source_run,source_terminal_seq,source_result_ref,actor,stage,NULL,plan_digest,request_digest,state,result_ref,workspace_ref,accepted_ns,updated_ns,terminal_ns`

const childExecutionColumns = `epoch,run_id,source_run,source_terminal_seq,source_result_ref,actor,stage,plan,plan_digest,request_digest,state,result_ref,workspace_ref,accepted_ns,updated_ns,terminal_ns`

func readChildExecution(ctx context.Context, reader childProposalReader, c ChildRecord, runID string) (ChildExecution, error) {
	return queryChildExecution(ctx, reader, c, runID, false)
}
func queryChildExecution(ctx context.Context, reader childProposalReader, c ChildRecord, runID string, metadataOnly bool) (ChildExecution, error) {
	columns := childExecutionColumns
	if metadataOnly {
		columns = childExecutionMetadataColumns
	}
	e := ChildExecution{Identity: c.Identity}
	var accepted, updated int64
	var terminal sql.NullInt64
	err := reader.QueryRowContext(ctx, `SELECT `+columns+` FROM child_execution_epochs WHERE child_id=? AND run_id=? AND length(plan)<=?`, c.ChildID, runID, MaxChildRestartPlanBytes).Scan(&e.Epoch, &e.RunID, &e.SourceRunID, &e.SourceTerminalSeq, &e.SourceResultRef, &e.Actor, &e.Stage, &e.Plan, &e.PlanDigest, &e.RequestDigest, &e.State, &e.ResultRef, &e.WorkspaceRef, &accepted, &updated, &terminal)
	if err != nil {
		return ChildExecution{}, err
	}
	e.AcceptedAt, e.UpdatedAt = time.Unix(0, accepted).UTC(), time.Unix(0, updated).UTC()
	if terminal.Valid {
		e.TerminalAt = time.Unix(0, terminal.Int64).UTC()
	}
	if e.Epoch > 0 {
		r := ChildRestartRequest{Identity: e.Identity, RunID: e.RunID, SourceRunID: e.SourceRunID, SourceTerminalSeq: e.SourceTerminalSeq, SourceResultRef: e.SourceResultRef, Actor: e.Actor, Stage: e.Stage, Plan: e.Plan, PlanDigest: e.PlanDigest}
		if e.Epoch > MaxChildExecutionEpochs || r.digest() != e.RequestDigest || (c.TombstonedAt.IsZero() && (!r.validMetadata(e.AcceptedAt) || (!metadataOnly && !r.valid(e.AcceptedAt)))) {
			return ChildExecution{}, ErrConflict
		}
	}
	if e.Epoch == 0 && (e.RunID != c.RunID || e.SourceRunID != "" || e.RequestDigest != "") {
		return ChildExecution{}, ErrConflict
	}
	return e, nil
}

// ChildExecution reads exact retained provenance within a qualified child.
func (s *Store) ChildExecution(ctx context.Context, id ChildIdentity, runID string) (ChildExecution, error) {
	c, err := s.GetChild(ctx, id)
	if err != nil {
		return ChildExecution{}, err
	}
	e, err := readChildExecution(ctx, s.db, c, runID)
	if errors.Is(err, sql.ErrNoRows) && c.ExecutionEpoch == 0 && runID == c.RunID {
		return initialChildExecution(c), nil
	}
	return e, err
}
func initialChildExecution(c ChildRecord) ChildExecution {
	return ChildExecution{Identity: c.Identity, RunID: c.RunID, State: c.State, ResultRef: c.ResultRef, WorkspaceRef: c.WorkspaceRef, AcceptedAt: c.AcceptedAt, UpdatedAt: c.UpdatedAt, TerminalAt: c.TerminalAt}
}

// ChildExecutionMetadata returns bounded identity/status without reading the
// retained plan BLOB. It verifies the metadata request digest, but is never an
// execution-authority check: callers that execute use ChildExecution instead.
func (s *Store) ChildExecutionMetadata(ctx context.Context, id ChildIdentity, runID string) (ChildExecution, error) {
	c, err := s.GetChild(ctx, id)
	if err != nil {
		return ChildExecution{}, err
	}
	e, err := queryChildExecution(ctx, s.db, c, runID, true)
	if errors.Is(err, sql.ErrNoRows) && c.ExecutionEpoch == 0 && runID == c.RunID {
		return initialChildExecution(c), nil
	}
	return e, err
}

// ChildExecutionHistory returns at most the original execution and eight human
// epochs. Payload reads are explicit through ChildExecution, keeping pages small.
func (s *Store) ChildExecutionHistory(ctx context.Context, id ChildIdentity) ([]ChildExecution, error) {
	c, err := s.GetChild(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.ExecutionEpoch == 0 {
		return []ChildExecution{initialChildExecution(c)}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id FROM child_execution_epochs WHERE child_id=? ORDER BY epoch LIMIT ?`, c.ChildID, MaxChildExecutionEpochs+1)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]ChildExecution, 0, len(ids))
	for _, runID := range ids {
		e, err := queryChildExecution(ctx, s.db, c, runID, true)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if len(out) != c.ExecutionEpoch+1 {
		return nil, ErrConflict
	}
	return out, nil
}

// ChildForExecutionRun maps any retained execution to its accepted child. It
// never implies that this execution remains current or may receive credentials.
// Callers must compare ActiveRunID before new effects, or select immutable prior
// custody explicitly. Tombstones preserve the mapping until family expiry.
func (s *Store) ChildForExecutionRun(ctx context.Context, runID string) (ChildRecord, error) {
	c, err := s.ChildForRun(ctx, runID)
	if !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	if !validChildText(runID, 256, true) {
		return ChildRecord{}, sql.ErrNoRows
	}
	c, err = scanChild(s.db.QueryRowContext(ctx, "SELECT "+childColumns+childFrom+` JOIN child_execution_epochs e ON e.child_id=c.child_id WHERE e.run_id=?`, runID))
	if err != nil {
		return ChildRecord{}, err
	}
	_, err = readChildExecution(ctx, s.db, c, runID)
	return c, err
}

func validateCurrentChildExecution(ctx context.Context, reader childProposalReader, c ChildRecord) error {
	if c.ExecutionEpoch == 0 {
		if c.ExecutionRunID != "" {
			return ErrConflict
		}
		return nil
	}
	e, err := readChildExecution(ctx, reader, c, c.ActiveRunID())
	if err != nil {
		return err
	}
	if e.Epoch != c.ExecutionEpoch || e.State != c.State || e.ResultRef != c.ResultRef || e.WorkspaceRef != c.WorkspaceRef || !e.UpdatedAt.Equal(c.UpdatedAt) {
		return ErrConflict
	}
	return nil
}
