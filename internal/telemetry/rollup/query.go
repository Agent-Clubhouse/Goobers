package rollup

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// RunSummary is a queryable row from the runs table (TEL-032).
type RunSummary struct {
	RunID           string
	Workflow        string
	WorkflowVersion int
	WorkflowDigest  string
	Gaggle          string
	TriggerKind     string
	TriggerRef      string
	Status          string
	StartedAt       time.Time
	FinishedAt      time.Time // zero if the run has not finished
	DurationMs      int64     // 0 if the run has not finished
}

// StageAttempt is a queryable row from the stage_attempts table.
type StageAttempt struct {
	Stage        string
	Attempt      int
	AttemptClass string
	Status       string
	StartedAt    time.Time
	FinishedAt   time.Time
	DurationMs   int64
	ErrorCode    string
	ErrorClass   string
}

// GateVerdict is a queryable row from the gate_verdicts table. RunnerJSON is
// the raw JSON text of Runner{repassAttempt, escalated} plus, for an agentic
// gate, a verdictRef{name, digest, size} pointer at the decision/rationale/
// evidence artifact (issue #128) — callers needing structured access should
// json.Unmarshal it; kept as text here rather than parsed, matching
// stage_attempts'/provider_mutations' runner_json convention.
type GateVerdict struct {
	Seq        uint64
	Gate       string
	Verdict    string
	Target     string
	OccurredAt time.Time
	RunnerJSON string
}

// ProviderMutation is a queryable row from the provider_mutations table.
type ProviderMutation struct {
	Seq        uint64
	Provider   string
	Kind       string
	ExternalID string
	URL        string
	Operation  string
	OccurredAt time.Time
}

// RunError is a queryable row from the run_errors table.
type RunError struct {
	Seq        uint64
	Stage      string
	Attempt    int
	Code       string
	ErrorClass string
	Message    string
	OccurredAt time.Time
}

// SpanSummary is a queryable row from the spans table.
type SpanSummary struct {
	SpanID        string
	ParentSpanID  string
	Name          string
	Kind          string
	Status        string
	StatusMessage string
	StartTime     time.Time
	EndTime       time.Time
	DurationMs    int64
}

// SpanEventSummary is a queryable row from the span_events table — a
// within-stage harness event attached to its stage span (TEL-012).
type SpanEventSummary struct {
	Seq        int
	Name       string
	OccurredAt time.Time
	Attributes map[string]string
}

// Runs returns every run in the rollup, ordered by start time then run id for
// a stable, comparable result set (rebuild-is-reproducible acceptance, #22).
func (db *DB) Runs() ([]RunSummary, error) {
	rows, err := db.sql.Query(`
		SELECT run_id, workflow, workflow_version, workflow_digest, gaggle, trigger_kind, trigger_ref, status, started_at, finished_at, duration_ms
		FROM runs ORDER BY started_at, run_id`)
	if err != nil {
		return nil, fmt.Errorf("rollup: query runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		var digest, triggerKind, triggerRef, status, startedAt, finishedAt sql.NullString
		var durationMs sql.NullInt64
		if err := rows.Scan(&r.RunID, &r.Workflow, &r.WorkflowVersion, &digest, &r.Gaggle,
			&triggerKind, &triggerRef, &status, &startedAt, &finishedAt, &durationMs); err != nil {
			return nil, fmt.Errorf("rollup: scan run: %w", err)
		}
		r.WorkflowDigest, r.TriggerKind, r.TriggerRef, r.Status = digest.String, triggerKind.String, triggerRef.String, status.String
		if r.StartedAt, err = parseTime(startedAt); err != nil {
			return nil, err
		}
		if r.FinishedAt, err = parseTime(finishedAt); err != nil {
			return nil, err
		}
		r.DurationMs = durationMs.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// StageAttempts returns every stage attempt for runID, ordered by stage then
// attempt number.
func (db *DB) StageAttempts(runID string) ([]StageAttempt, error) {
	rows, err := db.sql.Query(`
		SELECT stage, attempt, attempt_class, status, started_at, finished_at, duration_ms, error_code, error_class
		FROM stage_attempts WHERE run_id = ? ORDER BY stage, attempt`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query stage_attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StageAttempt
	for rows.Next() {
		var s StageAttempt
		var class, status, startedAt, finishedAt, errCode, errClass sql.NullString
		var durationMs sql.NullInt64
		if err := rows.Scan(&s.Stage, &s.Attempt, &class, &status, &startedAt, &finishedAt, &durationMs, &errCode, &errClass); err != nil {
			return nil, fmt.Errorf("rollup: scan stage_attempt: %w", err)
		}
		s.AttemptClass, s.Status, s.ErrorCode, s.ErrorClass = class.String, status.String, errCode.String, errClass.String
		if s.StartedAt, err = parseTime(startedAt); err != nil {
			return nil, err
		}
		if s.FinishedAt, err = parseTime(finishedAt); err != nil {
			return nil, err
		}
		s.DurationMs = durationMs.Int64
		out = append(out, s)
	}
	return out, rows.Err()
}

// GateVerdicts returns every gate evaluation for runID, in seq order.
func (db *DB) GateVerdicts(runID string) ([]GateVerdict, error) {
	rows, err := db.sql.Query(`
		SELECT seq, gate, verdict, target, occurred_at, runner_json FROM gate_verdicts
		WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query gate_verdicts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []GateVerdict
	for rows.Next() {
		var g GateVerdict
		var verdict, target, occurredAt, runnerJSON sql.NullString
		if err := rows.Scan(&g.Seq, &g.Gate, &verdict, &target, &occurredAt, &runnerJSON); err != nil {
			return nil, fmt.Errorf("rollup: scan gate_verdict: %w", err)
		}
		g.Verdict, g.Target, g.RunnerJSON = verdict.String, target.String, runnerJSON.String
		if g.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// HarnessTranscript is a queryable row from the harness_transcripts table — a
// pointer at a within-stage agent transcript/tool-output blob a harness
// executor recorded (journal.Run.RecordSpan, GBO-020); the blob itself stays
// content-addressed in the run journal's spans/ store (issue #128).
type HarnessTranscript struct {
	Seq        uint64
	Stage      string
	Name       string
	RefDigest  string
	RefSize    int64
	OccurredAt time.Time
}

// HarnessTranscripts returns every within-stage transcript pointer for runID,
// in seq order.
func (db *DB) HarnessTranscripts(runID string) ([]HarnessTranscript, error) {
	rows, err := db.sql.Query(`
		SELECT seq, stage, name, ref_digest, ref_size, occurred_at FROM harness_transcripts
		WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query harness_transcripts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []HarnessTranscript
	for rows.Next() {
		var h HarnessTranscript
		var digest, occurredAt sql.NullString
		var size sql.NullInt64
		if err := rows.Scan(&h.Seq, &h.Stage, &h.Name, &digest, &size, &occurredAt); err != nil {
			return nil, fmt.Errorf("rollup: scan harness_transcript: %w", err)
		}
		h.RefDigest, h.RefSize = digest.String, size.Int64
		if h.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SchedulerEvent is a queryable row from the scheduler_events table — a
// scheduler decision or claim-ledger transition from the instance journal
// (scheduler/events.jsonl), never per-run (issue #128).
type SchedulerEvent struct {
	Seq        uint64
	Type       string
	Workflow   string
	RunID      string
	Reason     string
	Status     string
	OccurredAt time.Time
}

// SchedulerEvents returns scheduler decisions in seq order, optionally
// filtered to one workflow (empty = every workflow) — "why didn't a run start
// at tick N" (issue #128).
func (db *DB) SchedulerEvents(workflow string) ([]SchedulerEvent, error) {
	query := `SELECT seq, type, workflow, run_id, reason, status, occurred_at FROM scheduler_events`
	args := []any{}
	if workflow != "" {
		query += ` WHERE workflow = ?`
		args = append(args, workflow)
	}
	query += ` ORDER BY seq`

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query scheduler_events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SchedulerEvent
	for rows.Next() {
		var e SchedulerEvent
		var wf, runID, reason, status, occurredAt sql.NullString
		if err := rows.Scan(&e.Seq, &e.Type, &wf, &runID, &reason, &status, &occurredAt); err != nil {
			return nil, fmt.Errorf("rollup: scan scheduler_event: %w", err)
		}
		e.Workflow, e.RunID, e.Reason, e.Status = wf.String, runID.String, reason.String, status.String
		if e.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ProviderMutations returns every external-ref-touched event for runID, in
// seq order — the traceable-mutation surface #12's MutationRecorder feeds.
func (db *DB) ProviderMutations(runID string) ([]ProviderMutation, error) {
	rows, err := db.sql.Query(`
		SELECT seq, provider, kind, external_id, url, operation, occurred_at FROM provider_mutations
		WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query provider_mutations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProviderMutation
	for rows.Next() {
		var m ProviderMutation
		var url, operation, occurredAt sql.NullString
		if err := rows.Scan(&m.Seq, &m.Provider, &m.Kind, &m.ExternalID, &url, &operation, &occurredAt); err != nil {
			return nil, fmt.Errorf("rollup: scan provider_mutation: %w", err)
		}
		m.URL, m.Operation = url.String, operation.String
		if m.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RunErrors returns every error event for runID, in seq order.
func (db *DB) RunErrors(runID string) ([]RunError, error) {
	rows, err := db.sql.Query(`
		SELECT seq, stage, attempt, code, error_class, message, occurred_at FROM run_errors
		WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query run_errors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunError
	for rows.Next() {
		var e RunError
		var stage, class, message, occurredAt sql.NullString
		var attempt sql.NullInt64
		if err := rows.Scan(&e.Seq, &stage, &attempt, &e.Code, &class, &message, &occurredAt); err != nil {
			return nil, fmt.Errorf("rollup: scan run_error: %w", err)
		}
		e.Stage, e.ErrorClass, e.Message = stage.String, class.String, message.String
		e.Attempt = int(attempt.Int64)
		if e.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Spans returns every span for runID, ordered by start time then span id.
func (db *DB) Spans(runID string) ([]SpanSummary, error) {
	rows, err := db.sql.Query(`
		SELECT span_id, parent_span_id, name, kind, status, status_message, start_time, end_time, duration_ms
		FROM spans WHERE run_id = ? ORDER BY start_time, span_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query spans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SpanSummary
	for rows.Next() {
		var s SpanSummary
		var parent, kind, statusMsg, start, end sql.NullString
		var durationMs sql.NullInt64
		if err := rows.Scan(&s.SpanID, &parent, &s.Name, &kind, &s.Status, &statusMsg, &start, &end, &durationMs); err != nil {
			return nil, fmt.Errorf("rollup: scan span: %w", err)
		}
		s.ParentSpanID, s.Kind, s.StatusMessage = parent.String, kind.String, statusMsg.String
		if s.StartTime, err = parseTime(start); err != nil {
			return nil, err
		}
		if s.EndTime, err = parseTime(end); err != nil {
			return nil, err
		}
		s.DurationMs = durationMs.Int64
		out = append(out, s)
	}
	return out, rows.Err()
}

// SpanEvents returns the within-stage harness events attached to spanID, in
// occurrence order — the granularity #22's acceptance criteria requires to
// survive rollup (queries return both stage-level and within-stage rows).
func (db *DB) SpanEvents(runID, spanID string) ([]SpanEventSummary, error) {
	rows, err := db.sql.Query(`
		SELECT seq, name, occurred_at, attributes_json FROM span_events
		WHERE run_id = ? AND span_id = ? ORDER BY seq`, runID, spanID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query span_events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SpanEventSummary
	for rows.Next() {
		var e SpanEventSummary
		var occurredAt, attrsJSON sql.NullString
		if err := rows.Scan(&e.Seq, &e.Name, &occurredAt, &attrsJSON); err != nil {
			return nil, fmt.Errorf("rollup: scan span_event: %w", err)
		}
		if e.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		if attrsJSON.Valid {
			if err := json.Unmarshal([]byte(attrsJSON.String), &e.Attributes); err != nil {
				return nil, fmt.Errorf("rollup: decode span_event attributes: %w", err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func parseTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, ns.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("rollup: parse timestamp %q: %w", ns.String, err)
	}
	return t, nil
}
