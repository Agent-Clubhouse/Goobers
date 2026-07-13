package rollup

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

// IngestRun reads a single run directory's journal (run.yaml + events.jsonl)
// and the telemetry span exporter's spans/spans.jsonl, and (re)populates the
// rollup's rows for that run. Ingestion is idempotent: existing rows for the
// run are deleted before the fresh insert, so IngestRun doubles as both
// incremental ingestion (call it once a run finishes — "hooks the runner",
// TEL-032) and the per-run primitive Rebuild uses to rederive the whole store
// from the journals (the rollup is derived state, never the source of truth).
func (db *DB) IngestRun(runDir string) error {
	identity, err := readRunIdentity(runDir)
	if err != nil {
		return err
	}
	events, err := readEvents(runDir)
	if err != nil {
		return err
	}
	spans, err := readSpans(runDir)
	if err != nil {
		return err
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("rollup: begin ingest tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	runID := identity.RunID
	if err := deleteRun(tx, runID); err != nil {
		return err
	}
	if err := insertRun(tx, identity, events); err != nil {
		return err
	}
	if err := insertEvents(tx, runID, events); err != nil {
		return err
	}
	if err := insertSpans(tx, runID, spans); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteRun(tx *sql.Tx, runID string) error {
	for _, table := range []string{"runs", "stage_attempts", "gate_verdicts", "provider_mutations", "run_errors", "spans", "span_events"} {
		if _, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE run_id = ?`, table), runID); err != nil {
			return fmt.Errorf("rollup: clear %s for run %s: %w", table, runID, err)
		}
	}
	return nil
}

func insertRun(tx *sql.Tx, id runIdentity, events []journalEvent) error {
	var status string
	var finishedAt time.Time
	for _, ev := range events {
		if ev.Type == eventRunFinished {
			status = ev.Status
			finishedAt = ev.Time
		}
	}
	_, err := tx.Exec(`
		INSERT INTO runs (run_id, workflow, workflow_version, workflow_digest, gaggle, trigger_kind, trigger_ref, status, started_at, finished_at, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.RunID, id.Workflow, id.WorkflowVersion, nullIfEmpty(id.WorkflowDigest), id.Gaggle,
		nullIfEmpty(id.Trigger.Kind), nullIfEmpty(id.Trigger.Ref), nullIfEmpty(status),
		formatTime(id.StartedAt), formatTime(finishedAt), durationMillis(id.StartedAt, finishedAt))
	if err != nil {
		return fmt.Errorf("rollup: insert run %s: %w", id.RunID, err)
	}
	return nil
}

// stageAttemptAccum consolidates fields spread across a stage attempt's
// stage.started / stage.finished / (optional) error events into one row.
type stageAttemptAccum struct {
	attemptClass          string
	status                string
	startedAt, finishedAt time.Time
	errorCode, errorClass string
	runnerJSON            sql.NullString
}

type stageKey struct {
	stage   string
	attempt int
}

func insertEvents(tx *sql.Tx, runID string, events []journalEvent) error {
	stages := map[stageKey]*stageAttemptAccum{}
	var order []stageKey
	getAccum := func(stage string, attempt int) *stageAttemptAccum {
		k := stageKey{stage, attempt}
		a, ok := stages[k]
		if !ok {
			a = &stageAttemptAccum{}
			stages[k] = a
			order = append(order, k)
		}
		return a
	}

	for _, ev := range events {
		switch ev.Type {
		case eventStageStarted:
			a := getAccum(ev.Stage, ev.Attempt)
			if ev.AttemptClass != "" {
				a.attemptClass = ev.AttemptClass
			}
			a.startedAt = ev.Time
			if rj, err := runnerJSON(ev.Runner); err != nil {
				return err
			} else if rj.Valid {
				a.runnerJSON = rj
			}

		case eventStageFinished:
			a := getAccum(ev.Stage, ev.Attempt)
			a.status = ev.Status
			a.finishedAt = ev.Time
			if rj, err := runnerJSON(ev.Runner); err != nil {
				return err
			} else if rj.Valid {
				a.runnerJSON = rj
			}

		case eventError:
			if ev.Error == nil {
				continue
			}
			class := string(telemetry.ClassifyError(ev.Error.Code))
			if _, err := tx.Exec(`
				INSERT INTO run_errors (run_id, seq, stage, attempt, code, error_class, message, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				runID, ev.Seq, nullIfEmpty(ev.Stage), nullIfZeroInt(ev.Attempt), ev.Error.Code,
				nullIfEmpty(class), nullIfEmpty(telemetry.Redact(ev.Error.Message)), formatTime(ev.Time)); err != nil {
				return fmt.Errorf("rollup: insert run_error seq %d: %w", ev.Seq, err)
			}
			if ev.Stage != "" {
				a := getAccum(ev.Stage, ev.Attempt)
				a.errorCode = ev.Error.Code
				a.errorClass = class
			}

		case eventGateEvaluated:
			if _, err := tx.Exec(`
				INSERT INTO gate_verdicts (run_id, seq, gate, verdict, target, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?)`,
				runID, ev.Seq, ev.Gate, nullIfEmpty(telemetry.Redact(ev.Verdict)), nullIfEmpty(ev.Target), formatTime(ev.Time)); err != nil {
				return fmt.Errorf("rollup: insert gate_verdict seq %d: %w", ev.Seq, err)
			}

		case eventRefTouched:
			if ev.ExternalRef == nil {
				continue
			}
			rj, err := runnerJSON(ev.Runner)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`
				INSERT INTO provider_mutations (run_id, seq, provider, kind, external_id, url, operation, occurred_at, runner_json)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				runID, ev.Seq, ev.ExternalRef.Provider, ev.ExternalRef.Kind, ev.ExternalRef.ID,
				nullIfEmpty(ev.ExternalRef.URL), nullIfEmpty(operationFromRunner(ev.Runner)), formatTime(ev.Time), rj); err != nil {
				return fmt.Errorf("rollup: insert provider_mutation seq %d: %w", ev.Seq, err)
			}
		}
	}

	for _, k := range order {
		a := stages[k]
		if _, err := tx.Exec(`
			INSERT INTO stage_attempts (run_id, stage, attempt, attempt_class, status, started_at, finished_at, duration_ms, error_code, error_class, runner_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, k.stage, k.attempt, nullIfEmpty(a.attemptClass), nullIfEmpty(a.status),
			formatTime(a.startedAt), formatTime(a.finishedAt), durationMillis(a.startedAt, a.finishedAt),
			nullIfEmpty(a.errorCode), nullIfEmpty(a.errorClass), a.runnerJSON); err != nil {
			return fmt.Errorf("rollup: insert stage_attempt %s/%d: %w", k.stage, k.attempt, err)
		}
	}
	return nil
}

func insertSpans(tx *sql.Tx, runID string, spans []telemetry.SpanRecord) error {
	for _, s := range spans {
		if _, err := tx.Exec(`
			INSERT INTO spans (run_id, span_id, parent_span_id, name, kind, status, status_message, start_time, end_time, duration_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, s.SpanID, nullIfEmpty(s.ParentSpanID), s.Name, nullIfEmpty(s.Kind), s.Status,
			nullIfEmpty(s.StatusMessage), formatTime(s.StartTime), formatTime(s.EndTime), durationMillis(s.StartTime, s.EndTime)); err != nil {
			return fmt.Errorf("rollup: insert span %s: %w", s.SpanID, err)
		}
		for i, ev := range s.Events {
			attrsJSON, err := marshalAttributes(ev.Attributes)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`
				INSERT INTO span_events (run_id, span_id, seq, name, occurred_at, attributes_json)
				VALUES (?, ?, ?, ?, ?, ?)`,
				runID, s.SpanID, i, ev.Name, formatTime(ev.Time), attrsJSON); err != nil {
				return fmt.Errorf("rollup: insert span_event %s/%d: %w", s.SpanID, i, err)
			}
		}
	}
	return nil
}
