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

// perRunTables lists every table keyed by run_id that IngestRun populates —
// deleteRun must clear all of them before a fresh insert, or a re-ingest
// (e.g. after a daemon restart resumes a run that already ingested once,
// issue #246) hits a stale row's primary key and rolls back the whole
// transaction. TestDeleteRunCoversEverySchemaTable guards against the next
// table added to insertEvents/insertSpans silently repeating this gap.
var perRunTables = []string{"runs", "stage_attempts", "gate_verdicts", "provider_mutations", "run_errors", "spans", "span_events", "harness_transcripts", "span_business_status"}

func deleteRun(tx *sql.Tx, runID string) error {
	for _, table := range perRunTables {
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

	// Pre-scan standalone error codes per stage/attempt so the
	// eventStageFinished case below can dedupe against them (issue #230)
	// regardless of where each event falls in the slice — a standalone
	// error event and stage.finished's own inline error are recorded for
	// genuinely different faults in practice (worktree_remove_failed vs a
	// business failure code) and both are wanted, but if the same code ever
	// shows up both ways for the same attempt, it must count once.
	standaloneErrorCodes := map[stageKey]map[string]bool{}
	for _, ev := range events {
		if ev.Type == eventError && ev.Error != nil && ev.Stage != "" {
			k := stageKey{ev.Stage, ev.Attempt}
			if standaloneErrorCodes[k] == nil {
				standaloneErrorCodes[k] = map[string]bool{}
			}
			standaloneErrorCodes[k][ev.Error.Code] = true
		}
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
			// Business failures (nonzero_exit, timeout, missing_result_file,
			// exec_start, result_file_path_escape) are recorded ONLY inline
			// here, never as a standalone error event (architect ruling,
			// #230) — derive a run_errors row from them the same way the
			// eventError case below does, or `telemetry errors` silently
			// misses this entire failure class despite `telemetry stats`
			// correctly counting it.
			if ev.Status == stageStatusFailure && ev.Error != nil {
				class := string(telemetry.ClassifyError(ev.Error.Code))
				a.errorCode = ev.Error.Code
				a.errorClass = class
				k := stageKey{ev.Stage, ev.Attempt}
				if !standaloneErrorCodes[k][ev.Error.Code] {
					if _, err := tx.Exec(`
						INSERT INTO run_errors (run_id, seq, stage, attempt, code, error_class, message, occurred_at)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
						runID, ev.Seq, nullIfEmpty(ev.Stage), nullIfZeroInt(ev.Attempt), ev.Error.Code,
						nullIfEmpty(class), nullIfEmpty(telemetry.Redact(ev.Error.Message)), formatTime(ev.Time)); err != nil {
						return fmt.Errorf("rollup: insert run_error (stage.finished) seq %d: %w", ev.Seq, err)
					}
				}
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
			// Runner{repassAttempt, escalated} plus a pointer at the verdict
			// artifact (decision/rationale/evidence, for agentic gates —
			// internal/gate/journal.go's recordVerdict) is exactly the
			// "gate X failed 3 repasses then escalated" signal Tutor/
			// nomination need (TUT-010 gate-noise family, issue #128) — v1
			// discarded both, leaving runner_json permanently NULL.
			rj, err := gateRunnerJSON(ev)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`
				INSERT INTO gate_verdicts (run_id, seq, gate, verdict, target, occurred_at, runner_json)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				runID, ev.Seq, ev.Gate, nullIfEmpty(telemetry.Redact(ev.Verdict)), nullIfEmpty(ev.Target), formatTime(ev.Time), rj); err != nil {
				return fmt.Errorf("rollup: insert gate_verdict seq %d: %w", ev.Seq, err)
			}

		case eventSpanRecorded:
			// Within-stage harness data (agent transcripts, tool output —
			// GBO-020) that v1 recorded to the journal via
			// journal.Run.RecordSpan but the rollup never ingested: the blob
			// itself stays content-addressed in the run journal's spans/
			// store (§3.3 excludes it from conformance, and it's often large
			// live-harness output) — this is a queryable pointer, not a copy.
			var digest sql.NullString
			var size sql.NullInt64
			if ev.Ref != nil {
				digest = nullIfEmpty(ev.Ref.Digest)
				size = nullIfZeroInt64(ev.Ref.Size)
			}
			if _, err := tx.Exec(`
				INSERT INTO harness_transcripts (run_id, seq, stage, name, ref_digest, ref_size, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				runID, ev.Seq, ev.Stage, ev.Name, digest, size, formatTime(ev.Time)); err != nil {
				return fmt.Errorf("rollup: insert harness_transcript seq %d: %w", ev.Seq, err)
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

// gateRunnerJSON merges a gate.evaluated event's Runner annotations
// (repassAttempt, escalated) with a pointer at its verdict artifact, if any
// (Name + Ref.Digest/Size — the decision/rationale/evidence blob an agentic
// gate recorded), into the one runner_json blob gate_verdicts stores. No new
// column: merging into the JSON runner_json already carries is cheaper than a
// migration and matches how stage_attempts/provider_mutations already stash
// their own runner-local detail there.
func gateRunnerJSON(ev journalEvent) (sql.NullString, error) {
	if len(ev.Runner) == 0 && ev.Ref == nil {
		return sql.NullString{}, nil
	}
	m := make(map[string]any, len(ev.Runner)+1)
	for k, v := range ev.Runner {
		m[k] = v
	}
	if ev.Ref != nil {
		m["verdictRef"] = map[string]any{"name": ev.Name, "digest": ev.Ref.Digest, "size": ev.Ref.Size}
	}
	return runnerJSON(m)
}

// IngestSchedulerLog reads the instance journal (scheduler/events.jsonl) —
// trigger.fired/tick.skipped/claim.acquired/claim.released, scheduler
// decisions, claim transitions, the instance-level run.started/run.finished
// echoes localscheduler's dispatch appends, and instance-level errors — and
// (re)populates scheduler_events (issue #128: this was never ingested at
// all). Idempotent (delete-then-insert over the whole table, since the
// instance log is a single stream, not per-run), so it's safe to call after
// every dispatch tick (incremental) or as part of Rebuild (full rescan).
// Historical duplicate seq values are corruption, but retaining the first
// occurrence keeps one bad record from permanently preventing all rollup.
func (db *DB) IngestSchedulerLog(schedulerDir string) error {
	events, err := readInstanceEvents(schedulerDir)
	if err != nil {
		return err
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("rollup: begin scheduler ingest tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	if _, err := tx.Exec(`DELETE FROM scheduler_events`); err != nil {
		return fmt.Errorf("rollup: clear scheduler_events: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM scheduler_errors`); err != nil {
		return fmt.Errorf("rollup: clear scheduler_errors: %w", err)
	}
	for _, ev := range events {
		switch ev.Type {
		case eventTriggerFired, eventTickSkipped, eventClaimAcquired, eventClaimReleased, eventRunStarted, eventRunFinished, eventError:
			if _, err := tx.Exec(`
				INSERT INTO scheduler_events (seq, type, workflow, run_id, reason, status, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(seq) DO NOTHING`,
				ev.Seq, ev.Type, nullIfEmpty(ev.Workflow), nullIfEmpty(ev.RunID), nullIfEmpty(ev.Reason), nullIfEmpty(ev.Status), formatTime(ev.Time)); err != nil {
				return fmt.Errorf("rollup: insert scheduler_event seq %d: %w", ev.Seq, err)
			}
			if ev.Type == eventError && ev.Error != nil {
				if _, err := tx.Exec(`
					INSERT INTO scheduler_errors (seq, code, error_class, message, occurred_at)
					VALUES (?, ?, ?, ?, ?)`,
					ev.Seq, ev.Error.Code, nullIfEmpty(string(telemetry.ClassifyError(ev.Error.Code))),
					nullIfEmpty(telemetry.Redact(ev.Error.Message)), formatTime(ev.Time)); err != nil {
					return fmt.Errorf("rollup: insert scheduler_error seq %d: %w", ev.Seq, err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rollup: commit scheduler ingest tx: %w", err)
	}
	// Checkpoint the WAL back into the main db file after every ingest
	// (#530 maintainer ruling). WAL mode already gives a separate reader
	// process (goobers telemetry/trace) correct read-after-commit
	// visibility without this — what an unchecked WAL actually risks is
	// unbounded file growth across thousands of incremental per-tick
	// ingests, and a non-WAL-aware tool (a raw copy of just the .db file)
	// missing not-yet-checkpointed rows. Best-effort only: this connection
	// is the sole writer (SetMaxOpenConns(1)), so nothing else can be
	// mid-write, but a concurrent reader transaction from another process
	// can legitimately hold the checkpoint back — that's a maintenance
	// delay, not a correctness problem, so its failure must never surface
	// as an ingest failure.
	checkpointWAL(db.sql)
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
		// businessStatus (issue #710) rides the span's own generic Attributes
		// map (Span.Complete sets it as an OTel attribute; JournalSpanExporter
		// already captures every attribute into SpanRecord.Attributes with no
		// exporter change needed) — a satellite row, not a spans column (see
		// schema.go's v3 migration comment): empty/absent for a span
		// predating this fix or one that never called Complete (a gate span,
		// still Succeed/Fail).
		if businessStatus := s.Attributes[telemetry.AttrBusinessStatus]; businessStatus != "" {
			if _, err := tx.Exec(`
				INSERT INTO span_business_status (run_id, span_id, business_status)
				VALUES (?, ?, ?)`,
				runID, s.SpanID, businessStatus); err != nil {
				return fmt.Errorf("rollup: insert span_business_status %s: %w", s.SpanID, err)
			}
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
