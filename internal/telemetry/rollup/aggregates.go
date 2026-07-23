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

// StatsRequest filters the aggregate views Stats returns. Model and
// HarnessVersion restrict results to agentic invocations; their GroupBy flags
// split run and stage rows into provenance cohorts. Run cohorts are
// participatory: a run using multiple grouped cohorts appears in each.
type StatsRequest struct {
	Workflow              string
	Gaggle                string
	Stage                 string
	Model                 string
	HarnessVersion        string
	GroupByModel          bool
	GroupByHarnessVersion bool
	Since                 time.Time
	Until                 time.Time
}

// GaggleStats is the success/failure/duration aggregate for one gaggle.
type GaggleStats struct {
	Gaggle        string  `json:"gaggle"`
	TotalRuns     int     `json:"totalRuns"`
	CompletedRuns int     `json:"completedRuns"`
	FailedRuns    int     `json:"failedRuns"`
	OtherRuns     int     `json:"otherRuns"`
	SuccessRate   float64 `json:"successRate"`
	AvgDurationMs float64 `json:"avgDurationMs"`
	MinDurationMs int64   `json:"minDurationMs"`
	MaxDurationMs int64   `json:"maxDurationMs"`
	HasDuration   bool    `json:"-"`
}

// RunStats is the success/failure/duration aggregate for one workflow.
type RunStats struct {
	Gaggle         string `json:"gaggle"`
	Workflow       string `json:"workflow"`
	Model          string `json:"model,omitempty"`
	HarnessVersion string `json:"harnessVersion,omitempty"`
	TotalRuns      int    `json:"totalRuns"`
	CompletedRuns  int    `json:"completedRuns"`
	FailedRuns     int    `json:"failedRuns"`
	OtherRuns      int    `json:"otherRuns"` // aborted, escalated, or still running
	// SuccessRate is CompletedRuns / (CompletedRuns + FailedRuns), the rate
	// over runs that have reached a success/failure verdict. 0 when neither
	// has occurred yet (avoids a divide-by-zero, not a claim of 0% success).
	SuccessRate   float64 `json:"successRate"`
	AvgDurationMs float64 `json:"avgDurationMs"`
	MinDurationMs int64   `json:"minDurationMs"`
	MaxDurationMs int64   `json:"maxDurationMs"`
	HasDuration   bool    `json:"-"`
}

// StageStats is the success/failure/duration aggregate for one stage identity.
type StageStats struct {
	Gaggle            string `json:"gaggle"`
	Workflow          string `json:"workflow"`
	Stage             string `json:"stage"`
	Model             string `json:"model,omitempty"`
	HarnessVersion    string `json:"harnessVersion,omitempty"`
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

// UsageStats is the AI usage aggregate for an instance, gaggle, workflow, or
// stage scope. Percentiles include only attempts that reported the resource.
type UsageStats struct {
	Scope          string `json:"scope"`
	Gaggle         string `json:"gaggle,omitempty"`
	Workflow       string `json:"workflow,omitempty"`
	Stage          string `json:"stage,omitempty"`
	Model          string `json:"model,omitempty"`
	HarnessVersion string `json:"harnessVersion,omitempty"`
	TotalAttempts  int    `json:"totalAttempts"`

	TokenSamples int   `json:"tokenSamples"`
	P50Tokens    int64 `json:"p50Tokens"`
	P95Tokens    int64 `json:"p95Tokens"`
	HasTokens    bool  `json:"-"`

	PremiumRequestSamples     int     `json:"premiumRequestSamples"`
	P50CopilotPremiumRequests float64 `json:"p50CopilotPremiumRequests"`
	P95CopilotPremiumRequests float64 `json:"p95CopilotPremiumRequests"`
	HasPremiumRequests        bool    `json:"-"`

	CostSamples int     `json:"costSamples"`
	P50CostUSD  float64 `json:"p50CostUSD"`
	P95CostUSD  float64 `json:"p95CostUSD"`
	HasCost     bool    `json:"-"`

	RetryWasteAttempts  int     `json:"retryWasteAttempts"`
	RetryWasteTokens    int64   `json:"retryWasteTokens"`
	RetryWasteCostUSD   float64 `json:"retryWasteCostUSD"`
	HasRetryWasteTokens bool    `json:"-"`
	HasRetryWasteCost   bool    `json:"-"`
}

// ModelStats is total observed usage grouped by model. Each measure carries its
// own sample count so absent usage is never reported as zero.
type ModelStats struct {
	Model                  string  `json:"model"`
	UsageSamples           int     `json:"usageSamples"`
	InputTokenSamples      int     `json:"inputTokenSamples"`
	InputTokens            int64   `json:"inputTokens"`
	HasInputTokens         bool    `json:"-"`
	OutputTokenSamples     int     `json:"outputTokenSamples"`
	OutputTokens           int64   `json:"outputTokens"`
	HasOutputTokens        bool    `json:"-"`
	PremiumRequestSamples  int     `json:"premiumRequestSamples"`
	CopilotPremiumRequests float64 `json:"copilotPremiumRequests"`
	HasPremiumRequests     bool    `json:"-"`
	CostSamples            int     `json:"costSamples"`
	CostUSD                float64 `json:"costUSD"`
	HasCost                bool    `json:"-"`
}

// StatsResult bundles the run-level and stage-level views a single Stats call
// returns.
type StatsResult struct {
	Gaggles []GaggleStats `json:"gaggles"`
	Runs    []RunStats    `json:"runs"`
	Stages  []StageStats  `json:"stages"`
	Usage   []UsageStats  `json:"usage"`
	Models  []ModelStats  `json:"models"`
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
	gaggles, err := db.gaggleStats(req)
	if err != nil {
		return StatsResult{}, err
	}
	runs, err := db.runStats(req)
	if err != nil {
		return StatsResult{}, err
	}
	stages, err := db.stageStats(req)
	if err != nil {
		return StatsResult{}, err
	}
	distributions, err := db.stageDistributionAccums(req)
	if err != nil {
		return StatsResult{}, err
	}
	populateStageDistributions(stages, distributions)
	usage, err := usageStats(distributions)
	if err != nil {
		return StatsResult{}, err
	}
	models, err := db.modelStats(req)
	if err != nil {
		return StatsResult{}, err
	}
	return StatsResult{Gaggles: gaggles, Runs: runs, Stages: stages, Usage: usage, Models: models}, nil
}

func (db *DB) gaggleStats(req StatsRequest) ([]GaggleStats, error) {
	where, args := statsWhere("workflow", "gaggle", "started_at", req)
	query := fmt.Sprintf(`
		SELECT gaggle,
			COUNT(*) AS total,
			SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS completed,
			SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS failed,
			AVG(duration_ms), MIN(duration_ms), MAX(duration_ms)
		FROM runs
		%s
		GROUP BY gaggle ORDER BY gaggle`, where)
	args = append([]any{runStatusCompleted, runStatusFailed}, args...)

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query gaggle stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []GaggleStats
	for rows.Next() {
		var s GaggleStats
		var avg sql.NullFloat64
		var min, max sql.NullInt64
		if err := rows.Scan(&s.Gaggle, &s.TotalRuns, &s.CompletedRuns, &s.FailedRuns, &avg, &min, &max); err != nil {
			return nil, fmt.Errorf("rollup: scan gaggle stats: %w", err)
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

func (db *DB) runStats(req StatsRequest) ([]RunStats, error) {
	where, whereArgs := statsWhere("r.workflow", "r.gaggle", "r.started_at", req)
	join, joinArgs := runAgentJoin(req)
	dimensions := agentDimensionColumns(req, "ai")
	selectDimensions := prefixedColumns(dimensions)
	groupDimensions := groupedColumns(dimensions)
	query := fmt.Sprintf(`
		SELECT r.gaggle, r.workflow%s,
			COUNT(*) AS total,
			SUM(CASE WHEN r.status = ? THEN 1 ELSE 0 END) AS completed,
			SUM(CASE WHEN r.status = ? THEN 1 ELSE 0 END) AS failed,
			AVG(r.duration_ms), MIN(r.duration_ms), MAX(r.duration_ms)
		FROM runs r
		%s
		%s
		GROUP BY r.gaggle, r.workflow%s ORDER BY r.gaggle, r.workflow%s`,
		selectDimensions, join, where, groupDimensions, groupDimensions)
	args := append([]any{runStatusCompleted, runStatusFailed}, joinArgs...)
	args = append(args, whereArgs...)

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
		scan := []any{&s.Gaggle, &s.Workflow}
		scan = appendAgentDimensionScan(scan, req, &s.Model, &s.HarnessVersion)
		scan = append(scan, &s.TotalRuns, &s.CompletedRuns, &s.FailedRuns, &avg, &min, &max)
		if err := rows.Scan(scan...); err != nil {
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
	clauses, args := statsClauses("r.workflow", "r.gaggle", "r.started_at", req)
	join := ""
	if agentStatsActive(req) {
		join = `JOIN agent_invocations ai
			ON ai.run_id = sa.run_id AND ai.stage = sa.stage AND ai.traversal = sa.traversal
			AND ai.kind = 'task'`
		agentClauses, agentArgs := agentFilterClauses("ai", req)
		clauses = append(clauses, agentClauses...)
		args = append(args, agentArgs...)
	}
	joinWhere := whereClause(clauses)
	dimensions := agentDimensionColumns(req, "ai")
	selectDimensions := prefixedColumns(dimensions)
	groupDimensions := groupedColumns(dimensions)
	query := fmt.Sprintf(`
		SELECT r.gaggle, r.workflow, sa.stage%s,
			COUNT(*) AS total,
			SUM(CASE WHEN sa.status = ? THEN 1 ELSE 0 END) AS succeeded,
			SUM(CASE WHEN sa.status = ? THEN 1 ELSE 0 END) AS failed,
			AVG(sa.duration_ms), MIN(sa.duration_ms), MAX(sa.duration_ms)
		FROM stage_attempts sa
		JOIN runs r ON r.run_id = sa.run_id
		%s
		%s
		GROUP BY r.gaggle, r.workflow, sa.stage%s
		ORDER BY r.gaggle, r.workflow, sa.stage%s`, selectDimensions, join, joinWhere, groupDimensions, groupDimensions)
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
		scan := []any{&s.Gaggle, &s.Workflow, &s.Stage}
		scan = appendAgentDimensionScan(scan, req, &s.Model, &s.HarnessVersion)
		scan = append(scan, &s.TotalAttempts, &s.SucceededAttempts, &s.FailedAttempts, &avg, &min, &max)
		if err := rows.Scan(scan...); err != nil {
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
	clauses, args := statsClauses(workflowCol, gaggleCol, timeCol, req)
	return whereClause(clauses), args
}

func statsClauses(workflowCol, gaggleCol, timeCol string, req StatsRequest) ([]string, []any) {
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
	return clauses, args
}

func whereClause(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(clauses, " AND ")
}

func agentStatsActive(req StatsRequest) bool {
	return req.Model != "" || req.HarnessVersion != "" || req.GroupByModel || req.GroupByHarnessVersion
}

func agentFilterClauses(alias string, req StatsRequest) ([]string, []any) {
	var clauses []string
	var args []any
	if req.Model != "" {
		clauses = append(clauses, alias+".model = ?")
		args = append(args, req.Model)
	}
	if req.HarnessVersion != "" {
		clauses = append(clauses, alias+".harness_version = ?")
		args = append(args, req.HarnessVersion)
	}
	return clauses, args
}

func agentDimensionColumns(req StatsRequest, alias string) []string {
	var columns []string
	if req.GroupByModel {
		columns = append(columns, alias+".model")
	}
	if req.GroupByHarnessVersion {
		columns = append(columns, alias+".harness_version")
	}
	return columns
}

func prefixedColumns(columns []string) string {
	if len(columns) == 0 {
		return ""
	}
	return ", " + strings.Join(columns, ", ")
}

func groupedColumns(columns []string) string {
	if len(columns) == 0 {
		return ""
	}
	return ", " + strings.Join(columns, ", ")
}

func appendAgentDimensionScan(scan []any, req StatsRequest, model, harnessVersion *string) []any {
	if req.GroupByModel {
		scan = append(scan, model)
	}
	if req.GroupByHarnessVersion {
		scan = append(scan, harnessVersion)
	}
	return scan
}

func runAgentJoin(req StatsRequest) (string, []any) {
	if !agentStatsActive(req) {
		return "", nil
	}
	columns := []string{"ai_source.run_id"}
	columns = append(columns, agentDimensionColumns(req, "ai_source")...)
	clauses, args := agentFilterClauses("ai_source", req)
	return fmt.Sprintf(
		"JOIN (SELECT DISTINCT %s FROM agent_invocations ai_source %s) ai ON ai.run_id = r.run_id",
		strings.Join(columns, ", "), whereClause(clauses),
	), args
}

// ErrorEvent is one run or instance error returned by the recent-errors query.
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

// ErrorsRequest filters recent errors. Empty code/class values are exact when
// their corresponding Filter field is true. Limit <= 0 defaults to 50.
type ErrorsRequest struct {
	Workflow         string
	Gaggle           string
	Stage            string
	Code             string
	ErrorClass       string
	FilterCode       bool
	FilterErrorClass bool
	Since            time.Time
	Until            time.Time
	Limit            int
	Cursor           *ErrorCursor
}

// ErrorCursor is the exclusive keyset boundary for the deterministic
// newest-first error ordering.
type ErrorCursor struct {
	OrderTimestamp string
	RunID          string
	Sequence       uint64
}

// Errors returns recent run and instance errors newest first. Run errors carry
// their run/stage reference; instance errors leave those fields empty.
// Filtering by ErrorClass also serves the mission brief's
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
		clauses = append(clauses, "e.workflow = ?")
		args = append(args, req.Workflow)
	}
	if req.Gaggle != "" {
		clauses = append(clauses, "e.gaggle = ?")
		args = append(args, req.Gaggle)
	}
	if req.Stage != "" {
		clauses = append(clauses, "e.stage = ?")
		args = append(args, req.Stage)
	}
	if req.FilterCode || req.Code != "" {
		clauses = append(clauses, "COALESCE(e.code, '') = ?")
		args = append(args, req.Code)
	}
	if req.FilterErrorClass || req.ErrorClass != "" {
		clauses = append(clauses, "COALESCE(e.error_class, '') = ?")
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
			(COALESCE(e.occurred_at, '') = ? AND COALESCE(e.run_id, '') < ?) OR
			(COALESCE(e.occurred_at, '') = ? AND COALESCE(e.run_id, '') = ? AND e.seq < ?))`)
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

	query := fmt.Sprintf(telemetryErrorsCTE+`
		SELECT e.run_id, e.workflow, e.stage, e.attempt, e.code, e.error_class, e.message, e.occurred_at, e.seq
		FROM telemetry_errors e
		%s
		ORDER BY COALESCE(e.occurred_at, '') DESC, COALESCE(e.run_id, '') DESC, e.seq DESC
		LIMIT ?`, where)

	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query errors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ErrorEvent
	for rows.Next() {
		var e ErrorEvent
		var runID, workflow, stage, class, message, occurredAt sql.NullString
		var attempt sql.NullInt64
		if err := rows.Scan(&runID, &workflow, &stage, &attempt, &e.Code, &class, &message, &occurredAt, &e.Sequence); err != nil {
			return nil, fmt.Errorf("rollup: scan error event: %w", err)
		}
		e.RunID, e.Workflow, e.Stage = runID.String, workflow.String, stage.String
		e.ErrorClass, e.Message = class.String, message.String
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
		SELECT e.seq, e.code, e.error_class, e.message, e.occurred_at, e.run_id, e.stage, e.attempt,
		       r.workflow, r.gaggle
		FROM run_errors e
		JOIN runs r ON r.run_id = e.run_id
		UNION ALL
		SELECT s.seq, s.code, s.error_class, s.message, s.occurred_at, NULL, NULL, NULL,
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
	where, args := errorSignaturesWhere(req)
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

	// The example row must respect the same scope/window filter as the
	// aggregate query above.
	exampleWhere, exampleArgs := errorSignaturesWhere(req)
	exampleFilter := "e.code = ? AND COALESCE(e.error_class, '') = ?"
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

func errorSignaturesWhere(req StatsRequest) (string, []any) {
	clauses, args := statsClauses("e.workflow", "e.gaggle", "e.occurred_at", req)
	if req.Stage != "" {
		clauses = append(clauses, "e.stage = ?")
		args = append(args, req.Stage)
	}
	return whereClause(clauses), args
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
