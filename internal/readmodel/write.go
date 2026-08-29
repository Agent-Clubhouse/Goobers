package readmodel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// UpsertRun writes a projection in ONE transaction.
//
// The run row, its stage rows, and (later) the change row and dirty-day marks
// commit together because §6.2 requires exactly that: they are atomic as a
// group, and they must stay in one database for that reason. A list that saw a
// run row without its stages would answer a stage-filtered query wrongly, and a
// change row published before the fact it describes would tell a client to
// refetch data that is not there yet.
//
// Idempotent on last_seq and terminal state: re-applying an unchanged projection
// is a no-op, while a newer projector may correct the interpretation of the same
// journal prefix. That distinction matters across binary upgrades: source
// position identifies the input, not the version of the pure projection code.
func (s *Store) UpsertRun(ctx context.Context, p Projection) error {
	// Merged before the transaction opens, so an unrelated store's latency never
	// lands on the read model's single writer (measurement.go).
	s.applyMeasurement(ctx, &p)

	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("readmodel: begin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read the prior row inside the transaction, before writing. It decides the
	// change kind, and reading it outside would race another writer between the
	// read and the write — classifying a progression as a creation, which a
	// client would act on by prepending a duplicate to its list.
	previous, existed, err := readRunRowTx(ctx, tx, p.Run.RunID)
	if err != nil {
		return err
	}
	// A projection behind the stored source position is a no-op, and a no-op must
	// not emit a change. Publishing one would wake every connected client to
	// refetch a row that did not move.
	//
	// At the stored position, phase and terminality are compared too. They are
	// derived facts and can legitimately change when a newer binary fixes its
	// projection rules without changing the immutable journal. An identical
	// replay still stops here, preserving idempotence and avoiding a spurious
	// change row; a corrected terminal interpretation proceeds and publishes the
	// transition once.
	//
	// Found by the backend-neutral conformance contract (#1921) on its first
	// run, which is the argument for having written it: nothing in the Wave 2
	// tests exercised a same-position replay, and the failure is invisible until
	// a client is connected to watch it.
	if existed && p.Run.LastSeq < previous.LastSeq {
		return nil
	}
	if existed && p.Run.LastSeq == previous.LastSeq &&
		p.Run.Phase == previous.Phase && p.Run.Terminal == previous.Terminal {
		return nil
	}

	if err := upsertRunRow(ctx, tx, p.Run); err != nil {
		return err
	}
	// Stage rows are replaced wholesale for this run rather than merged. A stage
	// set only grows within a run, so a delete-then-insert cannot lose data, and
	// it keeps the write idempotent under re-projection without needing to
	// reason about which stages a partial tail touched.
	if _, err := tx.ExecContext(ctx, `DELETE FROM run_stage WHERE run_id = ?`, p.Run.RunID); err != nil {
		return fmt.Errorf("readmodel: clear stages for %s: %w", p.Run.RunID, err)
	}
	for _, stage := range p.Stages {
		if err := insertStageRow(ctx, tx, stage, p.Run.Gaggle, p.Run.StartedAt, p.Run.Terminal); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM run_node WHERE run_id = ?`, p.Run.RunID); err != nil {
		return fmt.Errorf("readmodel: clear nodes for %s: %w", p.Run.RunID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM run_node_parent WHERE run_id = ?`, p.Run.RunID); err != nil {
		return fmt.Errorf("readmodel: clear node parents for %s: %w", p.Run.RunID, err)
	}
	for _, node := range p.Nodes {
		if err := insertNodeRow(ctx, tx, node); err != nil {
			return err
		}
	}
	for _, parent := range p.NodeParents {
		if err := insertNodeParentRow(ctx, tx, parent); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM remediation_example WHERE run_id = ?`, p.Run.RunID); err != nil {
		return fmt.Errorf("readmodel: clear remediation examples for %s: %w", p.Run.RunID, err)
	}
	for _, example := range p.Remediation {
		if err := insertRemediationExampleRow(ctx, tx, example); err != nil {
			return err
		}
	}
	// The change row commits WITH the fact it describes (§6.2). That ordering is
	// the point: today the projection updates on run finish while the stream
	// discovers change by polling the filesystem, so "refetch" and "the data is
	// there" can arrive out of order. Here they cannot.
	//
	// last_activity_at rather than time.Now(): the projection is a pure function
	// of the journal, and stamping a change with wall-clock time would make two
	// rebuilds of the same journals produce different feeds, breaking §14.9.
	at := p.Run.LastActivity
	if at.IsZero() {
		at = p.Run.StartedAt
	}
	if err := appendChange(ctx, tx, at, changeKindFor(previous, existed, p.Run), p.Run); err != nil {
		return err
	}
	// Queue the run's day for bucket recompute (#1931). One small insert inside
	// the transaction rather than an aggregation: recomputing here would put an
	// O(runs-in-day) scan on every run's commit path.
	if err := markDayDirty(ctx, tx, p.Run.StartedAt, s.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("readmodel: commit upsert: %w", err)
	}
	return nil
}

// readRunRowTx reads the columns needed to classify a transition, inside a
// transaction. Deliberately narrow: only what changeKindFor and the no-op guard
// consult, so it stays cheap on the write path.
func readRunRowTx(ctx context.Context, tx *sql.Tx, runID string) (RunRow, bool, error) {
	var out RunRow
	var terminal int
	err := tx.QueryRowContext(ctx,
		`SELECT run_id, phase, terminal, last_seq FROM run WHERE run_id = ?`, runID).
		Scan(&out.RunID, &out.Phase, &terminal, &out.LastSeq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return RunRow{}, false, nil
	case err != nil:
		return RunRow{}, false, fmt.Errorf("readmodel: read prior run %s: %w", runID, err)
	}
	out.Terminal = terminal != 0
	return out, true, nil
}

func upsertRunRow(ctx context.Context, tx *sql.Tx, row RunRow) error {
	disposition := row.Disposition
	if disposition == "" {
		disposition = DispositionUnknown
	}
	operatorJSON, err := json.Marshal(row.Operator)
	if err != nil {
		return fmt.Errorf("readmodel: encode operator facts for run %s: %w", row.RunID, err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO run (
			run_id, gaggle, workflow, workflow_version, workflow_digest, goober_digest,
			trigger_kind, trigger_ref, phase, terminal, current_stage,
			started_at, finished_at, last_activity_at, last_seq,
			repass_count, retry_count, policy_retry_count, infra_retry_count,
			outcome_verdict, outcome_target, disposition,
			any_token_measured, any_premium_measured, any_cost_measured, any_retry_waste,
			operator_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			gaggle = excluded.gaggle,
			workflow = excluded.workflow,
			workflow_version = excluded.workflow_version,
			workflow_digest = excluded.workflow_digest,
			goober_digest = excluded.goober_digest,
			trigger_kind = excluded.trigger_kind,
			trigger_ref = excluded.trigger_ref,
			phase = excluded.phase,
			terminal = excluded.terminal,
			current_stage = excluded.current_stage,
			started_at = excluded.started_at,
			finished_at = excluded.finished_at,
			last_activity_at = excluded.last_activity_at,
			last_seq = excluded.last_seq,
			repass_count = excluded.repass_count,
			retry_count = excluded.retry_count,
			policy_retry_count = excluded.policy_retry_count,
			infra_retry_count = excluded.infra_retry_count,
			outcome_verdict = excluded.outcome_verdict,
			outcome_target = excluded.outcome_target,
			disposition = excluded.disposition,
			any_token_measured = excluded.any_token_measured,
			any_premium_measured = excluded.any_premium_measured,
			any_cost_measured = excluded.any_cost_measured,
			any_retry_waste = excluded.any_retry_waste,
			operator_json = excluded.operator_json
		-- Idempotence, and the guard that makes out-of-order delivery safe: an
		-- older projection never overwrites a newer one. Without it, a repair
		-- sweep racing live projection could rewind a run's phase.
		WHERE excluded.last_seq >= run.last_seq`,
		row.RunID, row.Gaggle, row.Workflow, row.WorkflowVersion,
		nullString(row.WorkflowDigest), nullString(row.GooberDigest),
		nullString(row.TriggerKind), nullString(row.TriggerRef),
		string(row.Phase), boolInt(row.Terminal), nullString(row.CurrentStage),
		formatTime(row.StartedAt), nullTime(row.FinishedAt), nullTimeValue(row.LastActivity),
		row.LastSeq,
		row.RepassCount, row.RetryCount, row.PolicyRetryCount, row.InfraRetryCount,
		nullString(row.OutcomeVerdict), nullString(row.OutcomeTarget), disposition,
		boolInt(row.AnyTokenMeasured), boolInt(row.AnyPremiumMeasured),
		boolInt(row.AnyCostMeasured), boolInt(row.AnyRetryWaste),
		string(operatorJSON),
	)
	if err != nil {
		return fmt.Errorf("readmodel: upsert run %s: %w", row.RunID, err)
	}
	return nil
}

// insertStageRow writes one projected (run, stage) pair.
//
// gaggle and runStartedAt come from the RUN, not the stage, and are duplicated
// onto every stage row on purpose (#1782). A stage-filtered list is still
// gaggle-scoped and still ordered by run recency; without both here, the query
// drives from run_stage and then evaluates them against the joined run row --
// a residual predicate plus a sort, which is the shape §5.7 refuses.
//
// run_stage.started_at is the STAGE's own start, a different clock, so it cannot
// stand in for run recency. The copies cannot drift because these rows are
// deleted and rewritten wholesale on every projection of their run.
func insertStageRow(
	ctx context.Context,
	tx *sql.Tx,
	stage StageRow,
	gaggle string,
	runStartedAt time.Time,
	runTerminal bool,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO run_stage (
			run_id, stage, attempts, last_status, last_attempt_class, started_at, finished_at,
			gaggle, run_started_at, token_measured, premium_measured, cost_measured, retry_waste,
			had_success, had_failure, had_other, run_terminal
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stage.RunID, stage.Stage, stage.Attempts,
		nullString(stage.LastStatus), nullString(stage.LastAttemptClass),
		nullTime(stage.StartedAt), nullTime(stage.FinishedAt),
		gaggle, formatTime(runStartedAt),
		boolInt(stage.TokenMeasured), boolInt(stage.PremiumMeasured),
		boolInt(stage.CostMeasured), boolInt(stage.RetryWaste),
		boolInt(stage.HadSuccess), boolInt(stage.HadFailure), boolInt(stage.HadOther),
		boolInt(runTerminal),
	)
	if err != nil {
		return fmt.Errorf("readmodel: insert stage %s/%s: %w", stage.RunID, stage.Stage, err)
	}
	return nil
}

func insertNodeRow(ctx context.Context, tx *sql.Tx, node NodeRow) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO run_node (
			run_id, kind, name, identity, randomized, arm, attempts, retry_waste_attempts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		node.RunID, node.Kind, node.Name, node.Identity,
		boolInt(node.Randomized), node.Arm, node.Attempts, node.RetryWasteAttempts,
	)
	if err != nil {
		return fmt.Errorf("readmodel: insert node %s/%s: %w", node.RunID, node.Name, err)
	}
	return nil
}

func insertRemediationExampleRow(ctx context.Context, tx *sql.Tx, example RemediationExampleRow) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO remediation_example (
			run_id, stage, attempt, error_class, failure_excerpt, fix_excerpt, did_it_help, observed_at, config_digest
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		example.RunID, example.Stage, example.Attempt, example.ErrorClass,
		example.FailureExcerpt, example.FixExcerpt, boolInt(example.DidItHelp),
		nullTimeValue(example.ObservedAt), example.ConfigDigest,
	)
	if err != nil {
		return fmt.Errorf("readmodel: insert remediation example %s/%s/%d: %w",
			example.RunID, example.Stage, example.Attempt, err)
	}
	return nil
}

func insertNodeParentRow(ctx context.Context, tx *sql.Tx, parent NodeParentRow) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO run_node_parent (
			run_id, kind, name, identity, parent_kind, parent_name
		) VALUES (?, ?, ?, ?, ?, ?)`,
		parent.RunID, parent.Kind, parent.Name, parent.Identity, parent.ParentKind, parent.ParentName,
	)
	if err != nil {
		return fmt.Errorf("readmodel: insert node parent %s/%s: %w", parent.RunID, parent.Name, err)
	}
	return nil
}

// GetRun reads a projected row back.
//
// Scanned through runColumns/runScanTargets rather than its own column list.
// It used to carry a hand-written duplicate of both, which is precisely the
// shape that goes stale: adding any column to the run table meant remembering
// to edit two places, and forgetting the second produced a row that was silently
// missing a field rather than a compile error. #1782 added four columns and this
// is where they would have been dropped.
func (s *Store) GetRun(ctx context.Context, runID string) (RunRow, bool, error) {
	var out RunRow
	db, release, err := s.readHandle()
	if err != nil {
		return RunRow{}, false, err
	}
	row := db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM run r WHERE r.run_id = ?`, runID)
	switch err := row.Scan(runScanTargets(&out)...); {
	case errors.Is(err, sql.ErrNoRows):
		release()
		return RunRow{}, false, nil
	case err != nil:
		release()
		return RunRow{}, false, fmt.Errorf("readmodel: read run %s: %w", runID, err)
	}
	if err := out.finishScan(); err != nil {
		release()
		return RunRow{}, false, err
	}
	release()

	if out.Stages, err = s.runStages(ctx, runID); err != nil {
		return RunRow{}, false, err
	}
	return out, true, nil
}

// runStages returns the stage names recorded for a run, in stable order.
func (s *Store) runStages(ctx context.Context, runID string) ([]string, error) {
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx,
		`SELECT stage FROM run_stage WHERE run_id = ? ORDER BY stage`, runID)
	if err != nil {
		return nil, fmt.Errorf("readmodel: read stages for %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var stage string
		if err := rows.Scan(&stage); err != nil {
			return nil, fmt.Errorf("readmodel: scan stage: %w", err)
		}
		out = append(out, stage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: stage rows for %s: %w", runID, err)
	}
	return out, nil
}

// CountByPhase returns run counts grouped by phase.
//
// This is what §5.4 replaces the 17.2-second directory walk with: "stored, that
// becomes one indexed aggregate over phase = 'running'". It is served by
// idx_run_phase_recency without touching a journal.
func (s *Store) CountByPhase(ctx context.Context) (map[journal.RunPhase]int, error) {
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, `SELECT phase, COUNT(*) FROM run GROUP BY phase`)
	if err != nil {
		return nil, fmt.Errorf("readmodel: count by phase: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[journal.RunPhase]int{}
	for rows.Next() {
		var phase string
		var n int
		if err := rows.Scan(&phase, &n); err != nil {
			return nil, fmt.Errorf("readmodel: scan phase count: %w", err)
		}
		out[journal.RunPhase(phase)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: phase count rows: %w", err)
	}
	return out, nil
}

// ActiveRunCounts returns active counts grouped by workflow from the stored
// phase projection.
func (s *Store) ActiveRunCounts(ctx context.Context) ([]WorkflowCount, error) {
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, `
		SELECT gaggle, workflow, COUNT(*)
		FROM run
		WHERE phase = ?
		GROUP BY gaggle, workflow`, journal.PhaseRunning)
	if err != nil {
		return nil, fmt.Errorf("readmodel: count active runs by workflow: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WorkflowCount
	for rows.Next() {
		var count WorkflowCount
		if err := rows.Scan(&count.Gaggle, &count.Workflow, &count.Count); err != nil {
			return nil, fmt.Errorf("readmodel: scan active workflow count: %w", err)
		}
		out = append(out, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: active workflow count rows: %w", err)
	}
	return out, nil
}

// formatTime renders a timestamp in the store's fixed-width UTC layout, which is
// what makes lexicographic comparison a correct time comparison and lets
// started_at lead an ordering index.
func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func nullTimeValue(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// requiredTime parses a column that must be present.
func requiredTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return time.Time{}, fmt.Errorf("readmodel: missing required timestamp")
	}
	t, err := time.Parse(timeFormat, ns.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("readmodel: parse timestamp %q: %w", ns.String, err)
	}
	return t, nil
}
