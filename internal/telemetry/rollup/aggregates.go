package rollup

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Run status values, mirroring internal/journal.RunPhase's on-disk strings
// (not imported — same decoupling rationale as mirror.go).
const (
	runStatusCompleted = "completed"
	runStatusFailed    = "failed"
	runStatusAborted   = "aborted"
	runStatusEscalated = "escalated"
	runStatusRunning   = "running"
)

// Stage attempt status values, mirroring api/v1alpha1.ResultStatus's wire
// strings (a stable, long-merged contract package — safe to reference by
// value here without importing it, keeping this package free of the api/
// module's own dependency graph). "blocked" is the third value; like a
// non-terminal run status it falls out of StageStats by subtraction rather
// than a named branch.
const (
	stageStatusSuccess = "success"
	stageStatusFailure = "failure"
)

// StatsRequest filters the aggregate views Stats returns. Zero-value fields
// are unfiltered: empty Workflow/Gaggle matches every workflow/gaggle, zero
// Since/Until leaves that bound open. Gaggle exists because runs.gaggle does
// (TEL-Q4 per-gaggle partitioning) — issue #129: it was queryable in the
// schema but had no request field to filter on it.
type StatsRequest struct {
	Workflow string
	Gaggle   string
	Since    time.Time
	Until    time.Time
}

// RunStats is the success/failure/duration aggregate for one workflow.
type RunStats struct {
	Workflow      string `json:"workflow"`
	TotalRuns     int    `json:"totalRuns"`
	CompletedRuns int    `json:"completedRuns"`
	FailedRuns    int    `json:"failedRuns"`
	OtherRuns     int    `json:"otherRuns"` // aborted, escalated, or still running
	// SuccessRate is CompletedRuns / (CompletedRuns + FailedRuns), the rate
	// over runs that have reached a success/failure verdict. 0 when neither
	// has occurred yet (avoids a divide-by-zero, not a claim of 0% success).
	SuccessRate   float64 `json:"successRate"`
	AvgDurationMs float64 `json:"avgDurationMs"`
	MinDurationMs int64   `json:"minDurationMs"`
	MaxDurationMs int64   `json:"maxDurationMs"`
	HasDuration   bool    `json:"-"`
}

// StageStats is the success/failure/duration aggregate for one stage name,
// across every run matching the request's filters.
type StageStats struct {
	Stage             string `json:"stage"`
	TotalAttempts     int    `json:"totalAttempts"`
	SucceededAttempts int    `json:"succeededAttempts"`
	FailedAttempts    int    `json:"failedAttempts"`
	// SuccessRate is SucceededAttempts / (SucceededAttempts + FailedAttempts);
	// blocked attempts count toward TotalAttempts but not the rate (neither a
	// success nor a failure verdict).
	SuccessRate   float64 `json:"successRate"`
	AvgDurationMs float64 `json:"avgDurationMs"`
	MinDurationMs int64   `json:"minDurationMs"`
	MaxDurationMs int64   `json:"maxDurationMs"`
	HasDuration   bool    `json:"-"`

	DurationSamples int   `json:"durationSamples"`
	P50DurationMs   int64 `json:"p50DurationMs"`
	P95DurationMs   int64 `json:"p95DurationMs"`

	TokenSamples int   `json:"tokenSamples"`
	P50Tokens    int64 `json:"p50Tokens"`
	P95Tokens    int64 `json:"p95Tokens"`
	HasTokens    bool  `json:"-"`

	CostSamples int     `json:"costSamples"`
	P50CostUSD  float64 `json:"p50CostUSD"`
	P95CostUSD  float64 `json:"p95CostUSD"`
	HasCost     bool    `json:"-"`

	RetryWasteAttempts    int     `json:"retryWasteAttempts"`
	RetryWasteDurationMs  int64   `json:"retryWasteDurationMs"`
	RetryWasteTokens      int64   `json:"retryWasteTokens"`
	RetryWasteCostUSD     float64 `json:"retryWasteCostUSD"`
	HasRetryWasteDuration bool    `json:"-"`
	HasRetryWasteTokens   bool    `json:"-"`
	HasRetryWasteCost     bool    `json:"-"`
}

// StatsResult bundles the run-level and stage-level views a single Stats call
// returns — one query round-trip covers both the "by workflow" and "by stage"
// shapes #24 asks for.
type StatsResult struct {
	Runs   []RunStats   `json:"runs"`
	Stages []StageStats `json:"stages"`
}

// InstanceSummary is the lifetime (or Since-windowed) instance card exposed by
// `goobers stats`. SuccessRate follows RunStats: completed / (completed +
// failed), excluding phases that do not represent a success/failure verdict.
type InstanceSummary struct {
	TotalRuns     int
	CompletedRuns int
	FailedRuns    int
	AbortedRuns   int
	EscalatedRuns int
	RunningRuns   int
	OtherRuns     int
	SuccessRate   float64

	PullRequestsOpened int
	PullRequestsMerged int
	IssuesClaimed      int
	IssuesClosed       int

	BusiestWorkflow     string
	BusiestWorkflowRuns int

	AgenticStageAttempts      int
	AvgAgenticStageDurationMs float64
	LongestAgenticStageMs     int64
	LongestAgenticStage       string
	LongestAgenticWorkflow    string
	LongestAgenticRunID       string
}

// InstanceSummaryStats computes the one-screen instance summary. Run and
// workflow counts are windowed on runs.started_at, mutations on occurred_at,
// and agentic attempts on stage_attempts.started_at. A harness transcript is
// the existing rollup marker that a stage was agentic; deterministic stages do
// not invoke the harness or produce harness_transcripts rows.
func (db *DB) InstanceSummaryStats(since time.Time) (InstanceSummary, error) {
	var out InstanceSummary

	runWhere, runArgs := statsWhere("workflow", "gaggle", "started_at", StatsRequest{Since: since})
	runQuery := fmt.Sprintf(`
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0)
		FROM runs %s`, runWhere)
	args := append([]any{
		runStatusCompleted,
		runStatusFailed,
		runStatusAborted,
		runStatusEscalated,
		runStatusRunning,
	}, runArgs...)
	if err := db.sql.QueryRow(runQuery, args...).Scan(
		&out.TotalRuns,
		&out.CompletedRuns,
		&out.FailedRuns,
		&out.AbortedRuns,
		&out.EscalatedRuns,
		&out.RunningRuns,
	); err != nil {
		return InstanceSummary{}, fmt.Errorf("rollup: query instance run summary: %w", err)
	}
	out.OtherRuns = out.TotalRuns - out.CompletedRuns - out.FailedRuns - out.AbortedRuns - out.EscalatedRuns - out.RunningRuns
	if terminal := out.CompletedRuns + out.FailedRuns; terminal > 0 {
		out.SuccessRate = float64(out.CompletedRuns) / float64(terminal)
	}

	busiestQuery := fmt.Sprintf(`
		SELECT workflow, COUNT(*) AS run_count
		FROM runs %s
		GROUP BY workflow
		ORDER BY run_count DESC, workflow
		LIMIT 1`, runWhere)
	err := db.sql.QueryRow(busiestQuery, runArgs...).Scan(&out.BusiestWorkflow, &out.BusiestWorkflowRuns)
	if err != nil && err != sql.ErrNoRows {
		return InstanceSummary{}, fmt.Errorf("rollup: query busiest workflow: %w", err)
	}

	mutationWhere, mutationArgs := statsWhere("", "", "occurred_at", StatsRequest{Since: since})
	mutationQuery := fmt.Sprintf(`
		SELECT
			COALESCE(SUM(CASE WHEN kind = 'pr' AND operation = 'open' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'pr' AND operation = 'merge' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'issue' AND operation = 'claim' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'issue' AND operation = 'close' THEN 1 ELSE 0 END), 0)
		FROM provider_mutations %s`, mutationWhere)
	if err := db.sql.QueryRow(mutationQuery, mutationArgs...).Scan(
		&out.PullRequestsOpened,
		&out.PullRequestsMerged,
		&out.IssuesClaimed,
		&out.IssuesClosed,
	); err != nil {
		return InstanceSummary{}, fmt.Errorf("rollup: query instance mutation summary: %w", err)
	}

	stageFilter := `
		sa.duration_ms IS NOT NULL
		AND EXISTS (
			SELECT 1 FROM harness_transcripts h
			WHERE h.run_id = sa.run_id AND h.stage = sa.stage
		)`
	var stageArgs []any
	if !since.IsZero() {
		stageFilter += ` AND sa.started_at >= ?`
		stageArgs = append(stageArgs, formatTime(since).String)
	}
	stageQuery := fmt.Sprintf(`
		SELECT COUNT(*), COALESCE(AVG(sa.duration_ms), 0), COALESCE(MAX(sa.duration_ms), 0)
		FROM stage_attempts sa
		WHERE %s`, stageFilter)
	if err := db.sql.QueryRow(stageQuery, stageArgs...).Scan(
		&out.AgenticStageAttempts,
		&out.AvgAgenticStageDurationMs,
		&out.LongestAgenticStageMs,
	); err != nil {
		return InstanceSummary{}, fmt.Errorf("rollup: query agentic stage summary: %w", err)
	}
	if out.AgenticStageAttempts == 0 {
		return out, nil
	}

	longestQuery := fmt.Sprintf(`
		SELECT sa.stage, r.workflow, sa.run_id, sa.duration_ms
		FROM stage_attempts sa
		JOIN runs r ON r.run_id = sa.run_id
		WHERE %s
		ORDER BY sa.duration_ms DESC, sa.started_at, sa.run_id, sa.traversal
		LIMIT 1`, stageFilter)
	if err := db.sql.QueryRow(longestQuery, stageArgs...).Scan(
		&out.LongestAgenticStage,
		&out.LongestAgenticWorkflow,
		&out.LongestAgenticRunID,
		&out.LongestAgenticStageMs,
	); err != nil {
		return InstanceSummary{}, fmt.Errorf("rollup: query longest agentic stage: %w", err)
	}
	return out, nil
}

// Stats computes success/failure rates and durations by workflow and by
// stage, optionally filtered by workflow and/or a [Since, Until] time window
// on the run's start time (TEL-020/#24).
func (db *DB) Stats(req StatsRequest) (StatsResult, error) {
	runs, err := db.runStats(req)
	if err != nil {
		return StatsResult{}, err
	}
	stages, err := db.stageStats(req)
	if err != nil {
		return StatsResult{}, err
	}
	if err := db.populateStageDistributions(req, stages); err != nil {
		return StatsResult{}, err
	}
	return StatsResult{Runs: runs, Stages: stages}, nil
}

func (db *DB) runStats(req StatsRequest) ([]RunStats, error) {
	where, args := statsWhere("workflow", "gaggle", "started_at", req)
	query := fmt.Sprintf(`
		SELECT workflow,
			COUNT(*) AS total,
			SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS completed,
			SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS failed,
			AVG(duration_ms), MIN(duration_ms), MAX(duration_ms)
		FROM runs
		%s
		GROUP BY workflow ORDER BY workflow`, where)
	args = append([]any{runStatusCompleted, runStatusFailed}, args...)

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query run stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunStats
	for rows.Next() {
		var s RunStats
		var avg sql.NullFloat64
		var min, max sql.NullInt64
		if err := rows.Scan(&s.Workflow, &s.TotalRuns, &s.CompletedRuns, &s.FailedRuns, &avg, &min, &max); err != nil {
			return nil, fmt.Errorf("rollup: scan run stats: %w", err)
		}
		s.OtherRuns = s.TotalRuns - s.CompletedRuns - s.FailedRuns
		if terminal := s.CompletedRuns + s.FailedRuns; terminal > 0 {
			s.SuccessRate = float64(s.CompletedRuns) / float64(terminal)
		}
		s.AvgDurationMs, s.MinDurationMs, s.MaxDurationMs = avg.Float64, min.Int64, max.Int64
		s.HasDuration = avg.Valid
		out = append(out, s)
	}
	return out, rows.Err()
}

func (db *DB) stageStats(req StatsRequest) ([]StageStats, error) {
	// Stage attempts don't carry the workflow name directly; join to runs for
	// the workflow filter (and to keep the time window anchored on run start,
	// consistent with runStats — a stage's own started_at can be null for an
	// attempt that never started).
	joinWhere, args := statsWhere("r.workflow", "r.gaggle", "r.started_at", req)
	query := fmt.Sprintf(`
		SELECT sa.stage,
			COUNT(*) AS total,
			SUM(CASE WHEN sa.status = ? THEN 1 ELSE 0 END) AS succeeded,
			SUM(CASE WHEN sa.status = ? THEN 1 ELSE 0 END) AS failed,
			AVG(sa.duration_ms), MIN(sa.duration_ms), MAX(sa.duration_ms)
		FROM stage_attempts sa
		JOIN runs r ON r.run_id = sa.run_id
		%s
		GROUP BY sa.stage ORDER BY sa.stage`, joinWhere)
	args = append([]any{stageStatusSuccess, stageStatusFailure}, args...)

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query stage stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StageStats
	for rows.Next() {
		var s StageStats
		var avg sql.NullFloat64
		var min, max sql.NullInt64
		if err := rows.Scan(&s.Stage, &s.TotalAttempts, &s.SucceededAttempts, &s.FailedAttempts, &avg, &min, &max); err != nil {
			return nil, fmt.Errorf("rollup: scan stage stats: %w", err)
		}
		if terminal := s.SucceededAttempts + s.FailedAttempts; terminal > 0 {
			s.SuccessRate = float64(s.SucceededAttempts) / float64(terminal)
		}
		s.AvgDurationMs, s.MinDurationMs, s.MaxDurationMs = avg.Float64, min.Int64, max.Int64
		s.HasDuration = avg.Valid
		out = append(out, s)
	}
	return out, rows.Err()
}

// statsWhere builds a "WHERE ..." clause (or "" if unfiltered) plus its bind
// args for the given workflow/gaggle/time columns and request filters.
func statsWhere(workflowCol, gaggleCol, timeCol string, req StatsRequest) (string, []any) {
	var clauses []string
	var args []any
	if req.Workflow != "" {
		clauses = append(clauses, workflowCol+" = ?")
		args = append(args, req.Workflow)
	}
	if req.Gaggle != "" {
		clauses = append(clauses, gaggleCol+" = ?")
		args = append(args, req.Gaggle)
	}
	if !req.Since.IsZero() {
		clauses = append(clauses, timeCol+" >= ?")
		args = append(args, formatTime(req.Since).String)
	}
	if !req.Until.IsZero() {
		clauses = append(clauses, timeCol+" <= ?")
		args = append(args, formatTime(req.Until).String)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

// ErrorEvent is one run_errors row joined with its run's workflow, for the
// cross-run "recent errors" query.
type ErrorEvent struct {
	Sequence       uint64    `json:"-"`
	OrderTimestamp string    `json:"-"`
	RunID          string    `json:"runId"`
	Workflow       string    `json:"workflow"`
	Stage          string    `json:"stage"`
	Attempt        int       `json:"attempt"`
	Code           string    `json:"code"`
	ErrorClass     string    `json:"errorClass"`
	Message        string    `json:"message"`
	OccurredAt     time.Time `json:"occurredAt"`
}

// ErrorsRequest filters the cross-run recent-errors query. Zero-value fields
// are unfiltered. Limit <= 0 defaults to 50.
type ErrorsRequest struct {
	Workflow   string
	Gaggle     string
	ErrorClass string
	Since      time.Time
	Until      time.Time
	Limit      int
	Cursor     *ErrorCursor
}

// ErrorCursor is the exclusive keyset boundary for the deterministic
// newest-first error ordering.
type ErrorCursor struct {
	OrderTimestamp string
	RunID          string
	Sequence       uint64
}

// Errors returns recent errors across every run, newest first, each carrying
// its run/stage reference (#24's "recent errors by class with run/stage
// refs"). Filtering by ErrorClass also serves the mission brief's
// "rate-limit events" surface: Errors(ErrorsRequest{ErrorClass:
// string(telemetry.ErrorClassProviderRateLimit)}).
func (db *DB) Errors(req ErrorsRequest) ([]ErrorEvent, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	var clauses []string
	args := []any{}
	if req.Workflow != "" {
		clauses = append(clauses, "r.workflow = ?")
		args = append(args, req.Workflow)
	}
	if req.Gaggle != "" {
		clauses = append(clauses, "r.gaggle = ?")
		args = append(args, req.Gaggle)
	}
	if req.ErrorClass != "" {
		clauses = append(clauses, "e.error_class = ?")
		args = append(args, req.ErrorClass)
	}
	if !req.Since.IsZero() {
		clauses = append(clauses, "e.occurred_at >= ?")
		args = append(args, formatTime(req.Since).String)
	}
	if !req.Until.IsZero() {
		clauses = append(clauses, "e.occurred_at <= ?")
		args = append(args, formatTime(req.Until).String)
	}
	if req.Cursor != nil {
		occurredAt := req.Cursor.OrderTimestamp
		clauses = append(clauses, `(COALESCE(e.occurred_at, '') < ? OR
			(COALESCE(e.occurred_at, '') = ? AND e.run_id < ?) OR
			(COALESCE(e.occurred_at, '') = ? AND e.run_id = ? AND e.seq < ?))`)
		args = append(args,
			occurredAt,
			occurredAt, req.Cursor.RunID,
			occurredAt, req.Cursor.RunID, req.Cursor.Sequence,
		)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	args = append(args, limit)

	query := fmt.Sprintf(`
		SELECT e.run_id, r.workflow, e.stage, e.attempt, e.code, e.error_class, e.message, e.occurred_at, e.seq
		FROM run_errors e
		JOIN runs r ON r.run_id = e.run_id
		%s
		ORDER BY COALESCE(e.occurred_at, '') DESC, e.run_id DESC, e.seq DESC
		LIMIT ?`, where)

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query errors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ErrorEvent
	for rows.Next() {
		var e ErrorEvent
		var stage, class, message, occurredAt sql.NullString
		var attempt sql.NullInt64
		if err := rows.Scan(&e.RunID, &e.Workflow, &stage, &attempt, &e.Code, &class, &message, &occurredAt, &e.Sequence); err != nil {
			return nil, fmt.Errorf("rollup: scan error event: %w", err)
		}
		e.Stage, e.ErrorClass, e.Message = stage.String, class.String, message.String
		e.OrderTimestamp = occurredAt.String
		e.Attempt = int(attempt.Int64)
		if e.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ErrorSignature is one recurring (code, error_class) pattern across run and
// instance journals, with its occurrence count and a representative example.
type ErrorSignature struct {
	Code           string
	ErrorClass     string
	Count          int
	LastSeen       time.Time
	ExampleRunID   string
	ExampleStage   string
	ExampleAttempt int
}

const telemetryErrorsCTE = `
	WITH telemetry_errors AS (
		SELECT e.seq, e.code, e.error_class, e.occurred_at, e.run_id, e.stage, e.attempt,
		       r.workflow, r.gaggle
		FROM run_errors e
		JOIN runs r ON r.run_id = e.run_id
		UNION ALL
		SELECT s.seq, s.code, s.error_class, s.occurred_at, NULL, NULL, NULL,
		       NULL, NULL
		FROM scheduler_errors s
	)`

// TopErrorSignatures groups errors by (code, error_class), most frequent
// first, optionally filtered by workflow/time window. Instance-level errors
// are included in unscoped and time-scoped queries and excluded when a
// workflow or gaggle filter is present. limit<=0 defaults to 20.
func (db *DB) TopErrorSignatures(req StatsRequest, limit int) ([]ErrorSignature, error) {
	if limit <= 0 {
		limit = 20
	}
	where, args := statsWhere("e.workflow", "e.gaggle", "e.occurred_at", req)
	query := fmt.Sprintf(telemetryErrorsCTE+`
		SELECT e.code, e.error_class, COUNT(*) AS cnt, MAX(e.occurred_at) AS last_seen
		FROM telemetry_errors e
		%s
		GROUP BY e.code, e.error_class
		ORDER BY cnt DESC, e.code
		LIMIT ?`, where)
	args = append(args, limit)

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query error signatures: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sigs []ErrorSignature
	for rows.Next() {
		var sig ErrorSignature
		var class, lastSeen sql.NullString
		var count int
		if err := rows.Scan(&sig.Code, &class, &count, &lastSeen); err != nil {
			return nil, fmt.Errorf("rollup: scan error signature: %w", err)
		}
		sig.ErrorClass, sig.Count = class.String, count
		if sig.LastSeen, err = parseTime(lastSeen); err != nil {
			return nil, err
		}
		sigs = append(sigs, sig)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The example row must respect the same workflow/window filter as the
	// aggregate query above.
	exampleWhere, exampleArgs := statsWhere("e.workflow", "e.gaggle", "e.occurred_at", req)
	exampleFilter := "e.code = ? AND e.error_class = ?"
	if exampleWhere != "" {
		exampleFilter = strings.TrimPrefix(exampleWhere, "WHERE ") + " AND " + exampleFilter
	}
	for i := range sigs {
		var runID, stage sql.NullString
		var attempt sql.NullInt64
		args := append(append([]any{}, exampleArgs...), sigs[i].Code, sigs[i].ErrorClass)
		err := db.sql.QueryRow(fmt.Sprintf(telemetryErrorsCTE+`
			SELECT e.run_id, e.stage, e.attempt FROM telemetry_errors e
			WHERE %s
			ORDER BY e.occurred_at DESC, e.seq DESC LIMIT 1`, exampleFilter), args...).
			Scan(&runID, &stage, &attempt)
		if err != nil {
			return nil, fmt.Errorf("rollup: find example for signature %q: %w", sigs[i].Code, err)
		}
		sigs[i].ExampleRunID, sigs[i].ExampleStage, sigs[i].ExampleAttempt = runID.String, stage.String, int(attempt.Int64)
	}
	return sigs, nil
}

// ProviderMutationCount is the occurrence count of one (provider, kind,
// operation) mutation shape across every run.
type ProviderMutationCount struct {
	Provider  string
	Kind      string
	Operation string
	Count     int
}

// ProviderMutationCounts aggregates provider mutations across every run,
// optionally filtered by workflow/time window (#24's "provider-mutation
// counts").
func (db *DB) ProviderMutationCounts(req StatsRequest) ([]ProviderMutationCount, error) {
	where, args := statsWhere("r.workflow", "r.gaggle", "m.occurred_at", req)
	query := fmt.Sprintf(`
		SELECT m.provider, m.kind, COALESCE(m.operation, ''), COUNT(*) AS cnt
		FROM provider_mutations m
		JOIN runs r ON r.run_id = m.run_id
		%s
		GROUP BY m.provider, m.kind, m.operation
		ORDER BY cnt DESC, m.provider, m.kind`, where)

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query provider mutation counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProviderMutationCount
	for rows.Next() {
		var c ProviderMutationCount
		if err := rows.Scan(&c.Provider, &c.Kind, &c.Operation, &c.Count); err != nil {
			return nil, fmt.Errorf("rollup: scan provider mutation count: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
