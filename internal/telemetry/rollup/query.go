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
	Stage                  string
	Traversal              int
	Attempt                int
	Model                  string
	HarnessVersion         string
	AttemptClass           string
	Status                 string
	StartedAt              time.Time
	FinishedAt             time.Time
	DurationMs             int64
	ErrorCode              string
	ErrorClass             string
	InputTokens            *int64
	OutputTokens           *int64
	CopilotPremiumRequests *float64
	CostUSD                *float64
}

// AgentInvocation is model and harness provenance indexed from an agentic task
// or reviewer-gate span. Traversal and Attempt are nil for reviewer gates.
type AgentInvocation struct {
	SpanID         string
	Kind           string
	Stage          string
	Traversal      *int64
	Attempt        *int64
	Model          string
	HarnessVersion string
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
	SpanID        string `json:"spanId"`
	ParentSpanID  string `json:"parentSpanId,omitempty"`
	Name          string `json:"name"`
	Kind          string `json:"kind,omitempty"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage,omitempty"`
	// BusinessStatus is the run/stage's actual business outcome (issue #710:
	// success/failed/completed/escalated/aborted/blocked...), independent of
	// Status's coarser OTel ok/error axis. Empty for a span predating this
	// fix or one that never calls Span.Complete (a gate span).
	BusinessStatus string    `json:"businessStatus,omitempty"`
	StartTime      time.Time `json:"startTime"`
	EndTime        time.Time `json:"endTime"`
	DurationMs     int64     `json:"durationMs"`
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
// durable traversal number. Attempt numbers can restart at one after a repass.
func (db *DB) StageAttempts(runID string) ([]StageAttempt, error) {
	rows, err := db.sql.Query(`
		SELECT sa.stage, sa.traversal, sa.attempt, COALESCE(ai.model, ''), COALESCE(ai.harness_version, ''),
		       sa.attempt_class, sa.status, sa.started_at, sa.finished_at, sa.duration_ms,
		       sa.error_code, sa.error_class, su.input_tokens, su.output_tokens, su.copilot_premium_requests, su.cost_usd
		FROM stage_attempts sa
		LEFT JOIN stage_usage su
			ON su.run_id = sa.run_id AND su.stage = sa.stage AND su.traversal = sa.traversal
		LEFT JOIN agent_invocations ai
			ON ai.run_id = sa.run_id AND ai.stage = sa.stage AND ai.traversal = sa.traversal
			AND ai.kind = 'task'
		WHERE sa.run_id = ? ORDER BY sa.stage, sa.traversal`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query stage_attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StageAttempt
	for rows.Next() {
		var s StageAttempt
		var class, status, startedAt, finishedAt, errCode, errClass sql.NullString
		var durationMs, inputTokens, outputTokens sql.NullInt64
		var premiumRequests, costUSD sql.NullFloat64
		if err := rows.Scan(
			&s.Stage, &s.Traversal, &s.Attempt, &s.Model, &s.HarnessVersion,
			&class, &status, &startedAt, &finishedAt, &durationMs,
			&errCode, &errClass, &inputTokens, &outputTokens, &premiumRequests, &costUSD,
		); err != nil {
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
		s.InputTokens = optionalInt64(inputTokens)
		s.OutputTokens = optionalInt64(outputTokens)
		s.CopilotPremiumRequests = optionalFloat64(premiumRequests)
		s.CostUSD = optionalFloat64(costUSD)
		out = append(out, s)
	}
	return out, rows.Err()
}

// AgentInvocations returns every indexed agentic span for runID.
func (db *DB) AgentInvocations(runID string) ([]AgentInvocation, error) {
	rows, err := db.sql.Query(`
		SELECT span_id, kind, stage, traversal, attempt, model, harness_version
		FROM agent_invocations WHERE run_id = ? ORDER BY span_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query agent_invocations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AgentInvocation
	for rows.Next() {
		var invocation AgentInvocation
		var traversal, attempt sql.NullInt64
		if err := rows.Scan(
			&invocation.SpanID, &invocation.Kind, &invocation.Stage,
			&traversal, &attempt, &invocation.Model, &invocation.HarnessVersion,
		); err != nil {
			return nil, fmt.Errorf("rollup: scan agent_invocation: %w", err)
		}
		invocation.Traversal = optionalInt64(traversal)
		invocation.Attempt = optionalInt64(attempt)
		out = append(out, invocation)
	}
	return out, rows.Err()
}

func optionalInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func optionalFloat64(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
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

// HarnessTranscript is a queryable transcript pointer and its optional content
// schema. Empty Schema identifies a legacy unversioned row; the blob itself
// stays content-addressed in the run journal's spans/ store.
type HarnessTranscript struct {
	Seq        uint64
	Stage      string
	Name       string
	Schema     string
	RefDigest  string
	RefSize    int64
	OccurredAt time.Time
}

// HarnessTranscripts returns every within-stage transcript pointer for runID,
// in seq order.
func (db *DB) HarnessTranscripts(runID string) ([]HarnessTranscript, error) {
	rows, err := db.sql.Query(`
		SELECT h.seq, h.stage, h.name, COALESCE(s.schema, ''), h.ref_digest, h.ref_size, h.occurred_at
		FROM harness_transcripts h
		LEFT JOIN harness_transcript_schemas s ON s.run_id = h.run_id AND s.seq = h.seq
		WHERE h.run_id = ? ORDER BY h.seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query harness_transcripts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []HarnessTranscript
	for rows.Next() {
		var h HarnessTranscript
		var digest, occurredAt sql.NullString
		var size sql.NullInt64
		if err := rows.Scan(&h.Seq, &h.Stage, &h.Name, &h.Schema, &digest, &size, &occurredAt); err != nil {
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
	Seq          uint64
	Type         string
	Workflow     string
	RunID        string
	Reason       string
	Status       string
	ErrorCode    string
	ErrorClass   string
	ErrorMessage string
	OccurredAt   time.Time
}

// SchedulerEvents returns scheduler decisions in seq order, optionally
// filtered to one workflow (empty = every workflow) — "why didn't a run start
// at tick N" (issue #128).
func (db *DB) SchedulerEvents(workflow string) ([]SchedulerEvent, error) {
	query := `
		SELECT s.seq, s.type, s.workflow, s.run_id, s.reason, s.status,
		       e.code, e.error_class, e.message, s.occurred_at
		FROM scheduler_events s
		LEFT JOIN scheduler_errors e ON e.seq = s.seq`
	args := []any{}
	if workflow != "" {
		query += ` WHERE s.workflow = ?`
		args = append(args, workflow)
	}
	query += ` ORDER BY s.seq`

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query scheduler_events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SchedulerEvent
	for rows.Next() {
		var e SchedulerEvent
		var wf, runID, reason, status, errorCode, errorClass, errorMessage, occurredAt sql.NullString
		if err := rows.Scan(&e.Seq, &e.Type, &wf, &runID, &reason, &status, &errorCode, &errorClass, &errorMessage, &occurredAt); err != nil {
			return nil, fmt.Errorf("rollup: scan scheduler_event: %w", err)
		}
		e.Workflow, e.RunID, e.Reason, e.Status = wf.String, runID.String, reason.String, status.String
		e.ErrorCode, e.ErrorClass, e.ErrorMessage = errorCode.String, errorClass.String, errorMessage.String
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
		SELECT s.span_id, s.parent_span_id, s.name, s.kind, s.status, s.status_message, s.start_time, s.end_time, s.duration_ms, b.business_status
		FROM spans s LEFT JOIN span_business_status b ON b.run_id = s.run_id AND b.span_id = s.span_id
		WHERE s.run_id = ? ORDER BY s.start_time, s.span_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query spans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SpanSummary
	for rows.Next() {
		var s SpanSummary
		var parent, kind, statusMsg, start, end, businessStatus sql.NullString
		var durationMs sql.NullInt64
		if err := rows.Scan(&s.SpanID, &parent, &s.Name, &kind, &s.Status, &statusMsg, &start, &end, &durationMs, &businessStatus); err != nil {
			return nil, fmt.Errorf("rollup: scan span: %w", err)
		}
		s.ParentSpanID, s.Kind, s.StatusMessage, s.BusinessStatus = parent.String, kind.String, statusMsg.String, businessStatus.String
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
