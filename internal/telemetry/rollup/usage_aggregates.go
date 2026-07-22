package rollup

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
)

type stageDistributionAccum struct {
	durations []int64
	tokens    []int64
	costs     []float64

	wasteAttempts          int
	wasteDurationsObserved int
	wasteTokensObserved    int
	wasteCostsObserved     int
	wasteDurationMs        int64
	wasteTokens            int64
	wasteCostUSD           float64
}

type stageDistributionKey struct {
	gaggle         string
	workflow       string
	stage          string
	model          string
	harnessVersion string
}

func (db *DB) modelStats(req StatsRequest) ([]ModelStats, error) {
	where, args := statsWhere("r.workflow", "r.gaggle", "r.started_at", req)
	query := fmt.Sprintf(`
		SELECT smu.model,
		       COUNT(*),
		       COUNT(smu.input_tokens), COALESCE(SUM(smu.input_tokens), 0),
		       COUNT(smu.output_tokens), COALESCE(SUM(smu.output_tokens), 0),
		       COUNT(smu.copilot_premium_requests), COALESCE(SUM(smu.copilot_premium_requests), 0),
		       COUNT(smu.cost_usd), COALESCE(SUM(smu.cost_usd), 0)
		FROM stage_model_usage smu
		JOIN runs r ON r.run_id = smu.run_id
		%s
		GROUP BY smu.model
		ORDER BY smu.model`, where)
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query model usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ModelStats
	for rows.Next() {
		var stat ModelStats
		if err := rows.Scan(
			&stat.Model,
			&stat.UsageSamples,
			&stat.InputTokenSamples,
			&stat.InputTokens,
			&stat.OutputTokenSamples,
			&stat.OutputTokens,
			&stat.PremiumRequestSamples,
			&stat.CopilotPremiumRequests,
			&stat.CostSamples,
			&stat.CostUSD,
		); err != nil {
			return nil, fmt.Errorf("rollup: scan model usage: %w", err)
		}
		stat.HasInputTokens = stat.InputTokenSamples > 0
		stat.HasOutputTokens = stat.OutputTokenSamples > 0
		stat.HasPremiumRequests = stat.PremiumRequestSamples > 0
		stat.HasCost = stat.CostSamples > 0
		out = append(out, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rollup: iterate model usage: %w", err)
	}
	return out, nil
}

// populateStageDistributions adds nearest-rank p50/p95 measurements and retry
// waste to the existing stage rows. A retry-waste resource total is available
// only when every superseded attempt reported that resource, so missing usage
// can never be mistaken for zero.
func (db *DB) populateStageDistributions(req StatsRequest, stages []StageStats) error {
	if len(stages) == 0 {
		return nil
	}
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
	where := whereClause(clauses)
	dimensions := agentDimensionColumns(req, "ai")
	query := fmt.Sprintf(`
		SELECT r.gaggle, r.workflow, sa.stage%s,
		       sa.traversal < latest.final_traversal,
		       sa.duration_ms,
		       su.input_tokens,
		       su.output_tokens,
		       su.cost_usd
		FROM stage_attempts sa
		JOIN runs r ON r.run_id = sa.run_id
		%s
		LEFT JOIN stage_usage su
			ON su.run_id = sa.run_id AND su.stage = sa.stage AND su.traversal = sa.traversal
		JOIN (
			SELECT run_id, stage, MAX(traversal) AS final_traversal
			FROM stage_attempts
			GROUP BY run_id, stage
		) latest ON latest.run_id = sa.run_id AND latest.stage = sa.stage
		%s
		ORDER BY r.gaggle, r.workflow, sa.stage%s, sa.run_id, sa.traversal`, prefixedColumns(dimensions), join, where, groupedColumns(dimensions))
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return fmt.Errorf("rollup: query stage distributions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	accums := make(map[stageDistributionKey]*stageDistributionAccum, len(stages))
	for rows.Next() {
		var key stageDistributionKey
		var wasted bool
		var durationMs, inputTokens, outputTokens sql.NullInt64
		var costUSD sql.NullFloat64
		scan := []any{&key.gaggle, &key.workflow, &key.stage}
		scan = appendAgentDimensionScan(scan, req, &key.model, &key.harnessVersion)
		scan = append(scan, &wasted, &durationMs, &inputTokens, &outputTokens, &costUSD)
		if err := rows.Scan(scan...); err != nil {
			return fmt.Errorf("rollup: scan stage distribution: %w", err)
		}
		accum := accums[key]
		if accum == nil {
			accum = &stageDistributionAccum{}
			accums[key] = accum
		}
		if durationMs.Valid {
			if durationMs.Int64 < 0 {
				return fmt.Errorf("rollup: stage %s has negative duration %d", key.stage, durationMs.Int64)
			}
			accum.durations = append(accum.durations, durationMs.Int64)
		}
		var tokens int64
		hasTokens := inputTokens.Valid && outputTokens.Valid
		if hasTokens {
			if inputTokens.Int64 < 0 || outputTokens.Int64 < 0 {
				return fmt.Errorf("rollup: stage %s has negative token usage", key.stage)
			}
			tokens, err = addNonnegativeInt64(inputTokens.Int64, outputTokens.Int64)
			if err != nil {
				return fmt.Errorf("rollup: sum token usage for stage %s: %w", key.stage, err)
			}
			accum.tokens = append(accum.tokens, tokens)
		}
		if costUSD.Valid {
			if costUSD.Float64 < 0 || math.IsNaN(costUSD.Float64) || math.IsInf(costUSD.Float64, 0) {
				return fmt.Errorf("rollup: stage %s has invalid cost %v", key.stage, costUSD.Float64)
			}
			accum.costs = append(accum.costs, costUSD.Float64)
		}
		if !wasted {
			continue
		}
		accum.wasteAttempts++
		if durationMs.Valid {
			accum.wasteDurationsObserved++
			var err error
			accum.wasteDurationMs, err = addNonnegativeInt64(accum.wasteDurationMs, durationMs.Int64)
			if err != nil {
				return fmt.Errorf("rollup: sum retry-waste duration for stage %s: %w", key.stage, err)
			}
		}
		if hasTokens {
			accum.wasteTokensObserved++
			var err error
			accum.wasteTokens, err = addNonnegativeInt64(accum.wasteTokens, tokens)
			if err != nil {
				return fmt.Errorf("rollup: sum retry-waste tokens for stage %s: %w", key.stage, err)
			}
		}
		if costUSD.Valid {
			accum.wasteCostsObserved++
			accum.wasteCostUSD += costUSD.Float64
			if math.IsInf(accum.wasteCostUSD, 0) {
				return fmt.Errorf("rollup: retry-waste cost overflow for stage %s", key.stage)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rollup: iterate stage distributions: %w", err)
	}

	for i := range stages {
		key := stageDistributionKey{
			gaggle:         stages[i].Gaggle,
			workflow:       stages[i].Workflow,
			stage:          stages[i].Stage,
			model:          stages[i].Model,
			harnessVersion: stages[i].HarnessVersion,
		}
		accum := accums[key]
		if accum == nil {
			continue
		}
		stages[i].DurationSamples = len(accum.durations)
		if len(accum.durations) > 0 {
			stages[i].P50DurationMs = nearestRankInt64(accum.durations, 0.50)
			stages[i].P95DurationMs = nearestRankInt64(accum.durations, 0.95)
		}
		stages[i].TokenSamples = len(accum.tokens)
		if len(accum.tokens) > 0 {
			stages[i].HasTokens = true
			stages[i].P50Tokens = nearestRankInt64(accum.tokens, 0.50)
			stages[i].P95Tokens = nearestRankInt64(accum.tokens, 0.95)
		}
		stages[i].CostSamples = len(accum.costs)
		if len(accum.costs) > 0 {
			stages[i].HasCost = true
			stages[i].P50CostUSD = nearestRankFloat64(accum.costs, 0.50)
			stages[i].P95CostUSD = nearestRankFloat64(accum.costs, 0.95)
		}
		stages[i].RetryWasteAttempts = accum.wasteAttempts
		if accum.wasteAttempts > 0 && accum.wasteDurationsObserved == accum.wasteAttempts {
			stages[i].HasRetryWasteDuration = true
			stages[i].RetryWasteDurationMs = accum.wasteDurationMs
		}
		if accum.wasteAttempts > 0 && accum.wasteTokensObserved == accum.wasteAttempts {
			stages[i].HasRetryWasteTokens = true
			stages[i].RetryWasteTokens = accum.wasteTokens
		}
		if accum.wasteAttempts > 0 && accum.wasteCostsObserved == accum.wasteAttempts {
			stages[i].HasRetryWasteCost = true
			stages[i].RetryWasteCostUSD = accum.wasteCostUSD
		}
	}
	return nil
}

func nearestRankInt64(values []int64, percentile float64) int64 {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[nearestRank(len(values), percentile)]
}

func nearestRankFloat64(values []float64, percentile float64) float64 {
	sort.Float64s(values)
	return values[nearestRank(len(values), percentile)]
}

func nearestRank(length int, percentile float64) int {
	rank := int(math.Ceil(percentile*float64(length))) - 1
	if rank < 0 {
		return 0
	}
	if rank >= length {
		return length - 1
	}
	return rank
}

func addNonnegativeInt64(left, right int64) (int64, error) {
	if left < 0 || right < 0 || right > math.MaxInt64-left {
		return 0, fmt.Errorf("integer overflow")
	}
	return left + right, nil
}
