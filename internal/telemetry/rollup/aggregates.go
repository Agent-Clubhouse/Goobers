package rollup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
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

// runErrorCodeDurationExceeded mirrors runner.RunDurationExceededErrorCode's
// on-disk string (not imported — same decoupling rationale as the run status
// constants above). It identifies a run the watchdog aborted because it
// exceeded its configured maximum wall-clock duration (internal/runner's
// ExpireRun, which also fires during daemon startup before a stale run is
// resumed) — a run that hung and was later aborted, as opposed to one that
// reached a designed terminal on its own (#2534).
//
// runner.RunCanceledErrorCode (an operator's live `goobers run cancel`) is
// deliberately excluded from stuck-aborted classification: a cancel is an
// explicit action, not automatic stall detection, and folding it in would
// infer "stuck" from something other than the run's actual finalization
// reason. runner.RunStalledErrorCode is also excluded because the watchdog's
// stall path terminalizes to PhaseEscalated, not PhaseAborted — it never
// reaches the duration aggregates this excludes from.
//
// This reuses the error code already captured per-run in run_errors rather
// than adding new journal/producer plumbing, composing a minimal read-side
// slice of #1429's canonical terminal-outcome projection (and the typed
// terminal-cause record #463 defines, once it lands) instead of duplicating
// producer work — consistent with #1429's own design note to do exactly that.
const runErrorCodeDurationExceeded = "run_duration_exceeded"

// stuckAbortedRunsSubquery is a literal (no bind params — the code value is a
// package constant, not user input) subquery selecting the run_id of every
// run whose recorded terminal cause is runErrorCodeDurationExceeded. Joined
// against runs/stage_attempts to exclude stuck-then-aborted duration samples
// from percentiles/min/max while still counting them for denominator
// transparency (#1439's principle).
const stuckAbortedRunsSubquery = `SELECT DISTINCT run_id FROM run_errors WHERE code = '` + runErrorCodeDurationExceeded + `'`

// infraFailedRunsSubquery selects the run_id of every failed run whose
// terminal cause (the run_failed row — the one event failTerminal/
// finishStageFailure append exactly at the terminal, so a retried-then-
// recovered infra error can never match) classifies as an infrastructure
// fault: the producer's runner-namespace refinement projects the typed class
// onto that row (#3361). Literal, no bind params — every value is a package
// constant. The class list mirrors telemetry.ErrorClass.InfraFault, spelled
// from the same constants so the two cannot drift.
//
// Used to split FailedRuns into work failures and infra faults (#3364):
// "the credential was down" is not evidence against the lane, and a success
// rate that counts it as such misleads in only one direction. Rows written
// by producers older than the refinement carry no class and conservatively
// count as work failures.
const infraFailedRunsSubquery = `SELECT DISTINCT run_id FROM run_errors WHERE code = 'run_failed' AND error_class IN ('` +
	string(telemetry.ErrorClassInfra) + `', '` +
	string(telemetry.ErrorClassInfraGit) + `', '` +
	string(telemetry.ErrorClassInfraNet) + `', '` +
	string(telemetry.ErrorClassInfraLock) + `')`

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

// StatsRequest filters the aggregate views Stats returns. Branch filters stage
// and usage rows; GroupByBranch splits those rows into branch cohorts. Model and
// HarnessVersion restrict results to agentic invocations; their GroupBy flags
// split run and stage rows into provenance cohorts. Run cohorts are
// participatory: a run using multiple grouped agent cohorts appears in each.
type StatsRequest struct {
	Workflow              string
	Gaggle                string
	Stage                 string
	Branch                *int
	Model                 string
	HarnessVersion        string
	GroupByBranch         bool
	GroupByModel          bool
	GroupByHarnessVersion bool
	Since                 time.Time
	Until                 time.Time
	// Now pins the reference instant readyPoolHealth measures currently-open
	// implementation claims' in-flight age against (#2279). Zero defaults to
	// time.Now() — set explicitly only by tests that need a deterministic
	// age instead of a real wall-clock delta.
	Now time.Time
}

// ErrBranchAttributionRequiresRebuild means an upgraded rollup contains rows
// written before branch attribution was projected. Re-ingesting the journals
// is required before a branch-filtered or branch-grouped result can be exact.
var ErrBranchAttributionRequiresRebuild = errors.New("rollup: branch attribution is unknown for legacy rows; rebuild telemetry from journals")

// GaggleStats is the success/failure/duration aggregate for one gaggle.
type GaggleStats struct {
	Gaggle        string `json:"gaggle"`
	TotalRuns     int    `json:"totalRuns"`
	CompletedRuns int    `json:"completedRuns"`
	FailedRuns    int    `json:"failedRuns"`
	// InfraFailedRuns is how many of FailedRuns terminated on an
	// infrastructure fault (infraFailedRunsSubquery) — disclosed, and
	// excluded from SuccessRate's denominator (#3361/#3364).
	InfraFailedRuns int     `json:"infraFailedRuns"`
	OtherRuns       int     `json:"otherRuns"`
	SuccessRate     float64 `json:"successRate"`
	AvgDurationMs   float64 `json:"avgDurationMs"`
	MinDurationMs   int64   `json:"minDurationMs"`
	MaxDurationMs   int64   `json:"maxDurationMs"`
	HasDuration     bool    `json:"-"`
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
	// InfraFailedRuns is how many of FailedRuns terminated on an
	// infrastructure fault (infraFailedRunsSubquery): the substrate failed,
	// not the work, so they are disclosed here and excluded from
	// SuccessRate's denominator rather than silently scored against the
	// lane (#3361/#3364).
	InfraFailedRuns int `json:"infraFailedRuns"`
	OtherRuns       int `json:"otherRuns"` // aborted, escalated, or still running
	// SuccessRate is CompletedRuns / (CompletedRuns + FailedRuns −
	// InfraFailedRuns), the rate over runs that reached a success/failure
	// verdict ABOUT THE WORK — infra-fault terminals carry no such verdict
	// (#3364). 0 when the denominator is empty (avoids a divide-by-zero,
	// not a claim of 0% success).
	SuccessRate   float64 `json:"successRate"`
	AvgDurationMs float64 `json:"avgDurationMs"`
	MinDurationMs int64   `json:"minDurationMs"`
	MaxDurationMs int64   `json:"maxDurationMs"`
	HasDuration   bool    `json:"-"`
	// StuckAbortedRuns is how many of TotalRuns were excluded from
	// Avg/Min/MaxDurationMs because their recorded terminal cause is
	// runErrorCodeDurationExceeded (hung, then aborted) rather than a designed
	// terminal — disclosed rather than silently dropped (#2534, #1439).
	StuckAbortedRuns int `json:"stuckAbortedRuns"`
}

// StageStats is the success/failure/duration aggregate for one stage identity.
type StageStats struct {
	Gaggle            string `json:"gaggle"`
	Workflow          string `json:"workflow"`
	Stage             string `json:"stage"`
	Branch            *int   `json:"branch,omitempty"`
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
	// StuckAbortedAttempts is how many of TotalAttempts belong to a run whose
	// recorded terminal cause is runErrorCodeDurationExceeded, excluded from
	// Avg/Min/MaxDurationMs and from the percentile samples below — disclosed
	// rather than silently dropped (#2534, #1439).
	StuckAbortedAttempts int `json:"stuckAbortedAttempts"`

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
	Branch         *int   `json:"branch,omitempty"`
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
	CostUSD     float64 `json:"costUSD"`
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
	Gaggles   []GaggleStats   `json:"gaggles"`
	Runs      []RunStats      `json:"runs"`
	Stages    []StageStats    `json:"stages"`
	Usage     []UsageStats    `json:"usage"`
	Models    []ModelStats    `json:"models"`
	Curation  CurationStats   `json:"curation"`
	ReadyPool ReadyPoolHealth `json:"readyPool"`
}

// InstanceSummary is the lifetime (or Since-windowed) instance card exposed by
// `goobers stats`. SuccessRate follows RunStats: completed / (completed +
// failed − infraFailed), excluding phases and infra-fault terminals that do
// not represent a success/failure verdict about the work (#3364).
type InstanceSummary struct {
	TotalRuns     int
	CompletedRuns int
	FailedRuns    int
	// InfraFailedRuns is how many of FailedRuns terminated on an
	// infrastructure fault — disclosed, and excluded from SuccessRate's
	// denominator (#3361/#3364).
	InfraFailedRuns int
	AbortedRuns     int
	EscalatedRuns   int
	RunningRuns     int
	OtherRuns       int
	SuccessRate     float64

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
func (db *DB) InstanceSummaryStats(ctx context.Context, since time.Time) (InstanceSummary, error) {
	var out InstanceSummary

	runWhere, runArgs := statsWhere("workflow", "gaggle", "started_at", StatsRequest{Since: since})
	runQuery := fmt.Sprintf(`
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = ? AND ifr.run_id IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0)
		FROM runs
		LEFT JOIN (%s) ifr ON ifr.run_id = runs.run_id
		%s`, infraFailedRunsSubquery, runWhere)
	args := append([]any{
		runStatusCompleted,
		runStatusFailed,
		runStatusFailed,
		runStatusAborted,
		runStatusEscalated,
		runStatusRunning,
	}, runArgs...)
	if err := db.readDB().QueryRowContext(ctx, runQuery, args...).Scan(
		&out.TotalRuns,
		&out.CompletedRuns,
		&out.FailedRuns,
		&out.InfraFailedRuns,
		&out.AbortedRuns,
		&out.EscalatedRuns,
		&out.RunningRuns,
	); err != nil {
		return InstanceSummary{}, fmt.Errorf("rollup: query instance run summary: %w", err)
	}
	out.OtherRuns = out.TotalRuns - out.CompletedRuns - out.FailedRuns - out.AbortedRuns - out.EscalatedRuns - out.RunningRuns
	if terminal := out.CompletedRuns + out.FailedRuns - out.InfraFailedRuns; terminal > 0 {
		out.SuccessRate = float64(out.CompletedRuns) / float64(terminal)
	}

	busiestQuery := fmt.Sprintf(`
		SELECT workflow, COUNT(*) AS run_count
		FROM runs %s
		GROUP BY workflow
		ORDER BY run_count DESC, workflow
		LIMIT 1`, runWhere)
	err := db.readDB().QueryRowContext(ctx, busiestQuery, runArgs...).Scan(&out.BusiestWorkflow, &out.BusiestWorkflowRuns)
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
	if err := db.readDB().QueryRowContext(ctx, mutationQuery, mutationArgs...).Scan(
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
	if err := db.readDB().QueryRowContext(ctx, stageQuery, stageArgs...).Scan(
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
	if err := db.readDB().QueryRowContext(ctx, longestQuery, stageArgs...).Scan(
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
func (db *DB) Stats(ctx context.Context, req StatsRequest) (StatsResult, error) {
	if err := db.requireKnownBranchAttribution(ctx, req); err != nil {
		return StatsResult{}, err
	}
	gaggles, err := db.gaggleStats(ctx, req)
	if err != nil {
		return StatsResult{}, err
	}
	runs, err := db.runStats(ctx, req)
	if err != nil {
		return StatsResult{}, err
	}
	stages, err := db.stageStats(ctx, req)
	if err != nil {
		return StatsResult{}, err
	}
	distributions, err := db.stageDistributionAccums(ctx, req)
	if err != nil {
		return StatsResult{}, err
	}
	populateStageDistributions(stages, distributions)
	usage, err := usageStats(distributions, req.GroupByBranch || req.Branch != nil)
	if err != nil {
		return StatsResult{}, err
	}
	models, err := db.modelStats(ctx, req)
	if err != nil {
		return StatsResult{}, err
	}
	var readyTransitions []storedReadyLabelTransition
	if !agentStatsActive(req) && (req.Workflow == "" || req.Workflow == "backlog-curation") {
		readyTransitions, err = db.readyLabelTransitions(ctx, req)
		if err != nil {
			return StatsResult{}, err
		}
	}
	curation, err := db.curationStats(ctx, req, readyTransitions)
	if err != nil {
		return StatsResult{}, err
	}
	readyPool, err := db.readyPoolHealth(ctx, req, curation, readyTransitions)
	if err != nil {
		return StatsResult{}, err
	}
	return StatsResult{
		Gaggles: gaggles, Runs: runs, Stages: stages, Usage: usage, Models: models,
		Curation: curation, ReadyPool: readyPool,
	}, nil
}

func (db *DB) requireKnownBranchAttribution(ctx context.Context, req StatsRequest) error {
	if req.Branch == nil && !req.GroupByBranch {
		return nil
	}
	clauses, args := statsClauses("r.workflow", "r.gaggle", "r.started_at", req)
	clauses = append(clauses, "sa.branch IS NULL")
	query := fmt.Sprintf(`
		SELECT EXISTS (
			SELECT 1
			FROM stage_attempts sa
			JOIN runs r ON r.run_id = sa.run_id
			%s
		)`, whereClause(clauses))
	var unknown bool
	if err := db.readDB().QueryRowContext(ctx, query, args...).Scan(&unknown); err != nil {
		return fmt.Errorf("rollup: query unknown branch attribution: %w", err)
	}
	if unknown {
		return ErrBranchAttributionRequiresRebuild
	}
	return nil
}

func (db *DB) gaggleStats(ctx context.Context, req StatsRequest) ([]GaggleStats, error) {
	where, args := statsWhere("workflow", "gaggle", "started_at", req)
	query := fmt.Sprintf(`
		SELECT gaggle,
			COUNT(*) AS total,
			SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS completed,
			SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS failed,
			SUM(CASE WHEN status = ? AND ifr.run_id IS NOT NULL THEN 1 ELSE 0 END) AS infra_failed,
			AVG(duration_ms), MIN(duration_ms), MAX(duration_ms)
		FROM runs
		LEFT JOIN (%s) ifr ON ifr.run_id = runs.run_id
		%s
		GROUP BY gaggle ORDER BY gaggle`, infraFailedRunsSubquery, where)
	args = append([]any{runStatusCompleted, runStatusFailed, runStatusFailed}, args...)

	rows, err := db.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query gaggle stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []GaggleStats
	for rows.Next() {
		var s GaggleStats
		var avg sql.NullFloat64
		var min, max sql.NullInt64
		if err := rows.Scan(&s.Gaggle, &s.TotalRuns, &s.CompletedRuns, &s.FailedRuns, &s.InfraFailedRuns, &avg, &min, &max); err != nil {
			return nil, fmt.Errorf("rollup: scan gaggle stats: %w", err)
		}
		s.OtherRuns = s.TotalRuns - s.CompletedRuns - s.FailedRuns
		if terminal := s.CompletedRuns + s.FailedRuns - s.InfraFailedRuns; terminal > 0 {
			s.SuccessRate = float64(s.CompletedRuns) / float64(terminal)
		}
		s.AvgDurationMs, s.MinDurationMs, s.MaxDurationMs = avg.Float64, min.Int64, max.Int64
		s.HasDuration = avg.Valid
		out = append(out, s)
	}
	return out, rows.Err()
}

func (db *DB) runStats(ctx context.Context, req StatsRequest) ([]RunStats, error) {
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
			SUM(CASE WHEN r.status = ? AND ifr.run_id IS NOT NULL THEN 1 ELSE 0 END) AS infra_failed,
			AVG(CASE WHEN sar.run_id IS NULL THEN r.duration_ms END),
			MIN(CASE WHEN sar.run_id IS NULL THEN r.duration_ms END),
			MAX(CASE WHEN sar.run_id IS NULL THEN r.duration_ms END),
			COUNT(CASE WHEN sar.run_id IS NOT NULL THEN 1 END)
		FROM runs r
		LEFT JOIN (%s) sar ON sar.run_id = r.run_id
		LEFT JOIN (%s) ifr ON ifr.run_id = r.run_id
		%s
		%s
		GROUP BY r.gaggle, r.workflow%s ORDER BY r.gaggle, r.workflow%s`,
		selectDimensions, stuckAbortedRunsSubquery, infraFailedRunsSubquery, join, where, groupDimensions, groupDimensions)
	args := append([]any{runStatusCompleted, runStatusFailed, runStatusFailed}, joinArgs...)
	args = append(args, whereArgs...)

	rows, err := db.readDB().QueryContext(ctx, query, args...)
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
		scan = append(scan, &s.TotalRuns, &s.CompletedRuns, &s.FailedRuns, &s.InfraFailedRuns, &avg, &min, &max, &s.StuckAbortedRuns)
		if err := rows.Scan(scan...); err != nil {
			return nil, fmt.Errorf("rollup: scan run stats: %w", err)
		}
		s.OtherRuns = s.TotalRuns - s.CompletedRuns - s.FailedRuns
		if terminal := s.CompletedRuns + s.FailedRuns - s.InfraFailedRuns; terminal > 0 {
			s.SuccessRate = float64(s.CompletedRuns) / float64(terminal)
		}
		s.AvgDurationMs, s.MinDurationMs, s.MaxDurationMs = avg.Float64, min.Int64, max.Int64
		s.HasDuration = avg.Valid
		out = append(out, s)
	}
	return out, rows.Err()
}

func (db *DB) stageStats(ctx context.Context, req StatsRequest) ([]StageStats, error) {
	// Stage attempts don't carry the workflow name directly; join to runs for
	// the workflow filter (and to keep the time window anchored on run start,
	// consistent with runStats — a stage's own started_at can be null for an
	// attempt that never started).
	clauses, args := statsClauses("r.workflow", "r.gaggle", "r.started_at", req)
	branchClauses, branchArgs := branchFilterClauses("sa", req)
	clauses = append(clauses, branchClauses...)
	args = append(args, branchArgs...)
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
	dimensions := stageDimensionColumns(req, "sa", "ai")
	selectDimensions := prefixedColumns(dimensions)
	groupDimensions := groupedColumns(dimensions)
	query := fmt.Sprintf(`
		SELECT r.gaggle, r.workflow, sa.stage%s,
			COUNT(*) AS total,
			SUM(CASE WHEN sa.status = ? THEN 1 ELSE 0 END) AS succeeded,
			SUM(CASE WHEN sa.status = ? THEN 1 ELSE 0 END) AS failed,
			AVG(CASE WHEN sar.run_id IS NULL THEN sa.duration_ms END),
			MIN(CASE WHEN sar.run_id IS NULL THEN sa.duration_ms END),
			MAX(CASE WHEN sar.run_id IS NULL THEN sa.duration_ms END),
			COUNT(CASE WHEN sar.run_id IS NOT NULL THEN 1 END)
		FROM stage_attempts sa
		JOIN runs r ON r.run_id = sa.run_id
		LEFT JOIN (%s) sar ON sar.run_id = r.run_id
		%s
		%s
		GROUP BY r.gaggle, r.workflow, sa.stage%s
		ORDER BY r.gaggle, r.workflow, sa.stage%s`, selectDimensions, stuckAbortedRunsSubquery, join, joinWhere, groupDimensions, groupDimensions)
	args = append([]any{stageStatusSuccess, stageStatusFailure}, args...)

	rows, err := db.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query stage stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StageStats
	for rows.Next() {
		var s StageStats
		var avg sql.NullFloat64
		var min, max sql.NullInt64
		var branch sql.NullInt64
		scan := []any{&s.Gaggle, &s.Workflow, &s.Stage}
		scan = appendStageDimensionScan(scan, req, &branch, &s.Model, &s.HarnessVersion)
		scan = append(scan, &s.TotalAttempts, &s.SucceededAttempts, &s.FailedAttempts, &avg, &min, &max, &s.StuckAbortedAttempts)
		if err := rows.Scan(scan...); err != nil {
			return nil, fmt.Errorf("rollup: scan stage stats: %w", err)
		}
		if req.GroupByBranch && branch.Valid {
			value := int(branch.Int64)
			s.Branch = &value
		} else if req.Branch != nil {
			filteredBranch := *req.Branch
			s.Branch = &filteredBranch
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

func stageDimensionColumns(req StatsRequest, stageAlias, agentAlias string) []string {
	var columns []string
	if req.GroupByBranch {
		columns = append(columns, stageAlias+".branch")
	}
	return append(columns, agentDimensionColumns(req, agentAlias)...)
}

func branchFilterClauses(alias string, req StatsRequest) ([]string, []any) {
	if req.Branch == nil {
		return nil, nil
	}
	return []string{alias + ".branch = ?"}, []any{*req.Branch}
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

func appendStageDimensionScan(scan []any, req StatsRequest, branch *sql.NullInt64, model, harnessVersion *string) []any {
	if req.GroupByBranch {
		scan = append(scan, branch)
	}
	return appendAgentDimensionScan(scan, req, model, harnessVersion)
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
func (db *DB) Errors(ctx context.Context, req ErrorsRequest) ([]ErrorEvent, error) {
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

	rows, err := db.readDB().QueryContext(ctx, query, args...)
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
func (db *DB) TopErrorSignatures(ctx context.Context, req StatsRequest, limit int) ([]ErrorSignature, error) {
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

	rows, err := db.readDB().QueryContext(ctx, query, args...)
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
		err := db.readDB().QueryRowContext(ctx, fmt.Sprintf(telemetryErrorsCTE+`
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
func (db *DB) ProviderMutationCounts(ctx context.Context, req StatsRequest) ([]ProviderMutationCount, error) {
	where, args := statsWhere("r.workflow", "r.gaggle", "m.occurred_at", req)
	query := fmt.Sprintf(`
		SELECT m.provider, m.kind, COALESCE(m.operation, ''), COUNT(*) AS cnt
		FROM provider_mutations m
		JOIN runs r ON r.run_id = m.run_id
		%s
		GROUP BY m.provider, m.kind, m.operation
		ORDER BY cnt DESC, m.provider, m.kind`, where)

	rows, err := db.readDB().QueryContext(ctx, query, args...)
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
