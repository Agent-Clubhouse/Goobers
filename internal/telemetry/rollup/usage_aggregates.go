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

// populateStageDistributions adds nearest-rank p50/p95 measurements and retry
// waste to the existing stage rows. A retry-waste resource total is available
// only when every superseded attempt reported that resource, so missing usage
// can never be mistaken for zero.
func (db *DB) populateStageDistributions(req StatsRequest, stages []StageStats) error {
	if len(stages) == 0 {
		return nil
	}
	where, args := statsWhere("r.workflow", "r.gaggle", "r.started_at", req)
	query := fmt.Sprintf(`
		SELECT sa.stage, sa.attempt < latest.final_attempt,
		       sa.duration_ms,
		       su.input_tokens,
		       su.output_tokens,
		       su.cost_usd
		FROM stage_attempts sa
		JOIN runs r ON r.run_id = sa.run_id
		LEFT JOIN stage_usage su
			ON su.run_id = sa.run_id AND su.stage = sa.stage AND su.attempt = sa.attempt
		JOIN (
			SELECT run_id, stage, MAX(attempt) AS final_attempt
			FROM stage_attempts
			GROUP BY run_id, stage
		) latest ON latest.run_id = sa.run_id AND latest.stage = sa.stage
		%s
		ORDER BY sa.stage, sa.run_id, sa.attempt`, where)
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return fmt.Errorf("rollup: query stage distributions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	accums := make(map[string]*stageDistributionAccum, len(stages))
	for rows.Next() {
		var stage string
		var wasted bool
		var durationMs, inputTokens, outputTokens sql.NullInt64
		var costUSD sql.NullFloat64
		if err := rows.Scan(&stage, &wasted, &durationMs, &inputTokens, &outputTokens, &costUSD); err != nil {
			return fmt.Errorf("rollup: scan stage distribution: %w", err)
		}
		accum := accums[stage]
		if accum == nil {
			accum = &stageDistributionAccum{}
			accums[stage] = accum
		}
		if durationMs.Valid {
			if durationMs.Int64 < 0 {
				return fmt.Errorf("rollup: stage %s has negative duration %d", stage, durationMs.Int64)
			}
			accum.durations = append(accum.durations, durationMs.Int64)
		}
		var tokens int64
		hasTokens := inputTokens.Valid && outputTokens.Valid
		if hasTokens {
			if inputTokens.Int64 < 0 || outputTokens.Int64 < 0 {
				return fmt.Errorf("rollup: stage %s has negative token usage", stage)
			}
			tokens, err = addNonnegativeInt64(inputTokens.Int64, outputTokens.Int64)
			if err != nil {
				return fmt.Errorf("rollup: sum token usage for stage %s: %w", stage, err)
			}
			accum.tokens = append(accum.tokens, tokens)
		}
		if costUSD.Valid {
			if costUSD.Float64 < 0 || math.IsNaN(costUSD.Float64) || math.IsInf(costUSD.Float64, 0) {
				return fmt.Errorf("rollup: stage %s has invalid cost %v", stage, costUSD.Float64)
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
				return fmt.Errorf("rollup: sum retry-waste duration for stage %s: %w", stage, err)
			}
		}
		if hasTokens {
			accum.wasteTokensObserved++
			var err error
			accum.wasteTokens, err = addNonnegativeInt64(accum.wasteTokens, tokens)
			if err != nil {
				return fmt.Errorf("rollup: sum retry-waste tokens for stage %s: %w", stage, err)
			}
		}
		if costUSD.Valid {
			accum.wasteCostsObserved++
			accum.wasteCostUSD += costUSD.Float64
			if math.IsInf(accum.wasteCostUSD, 0) {
				return fmt.Errorf("rollup: retry-waste cost overflow for stage %s", stage)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rollup: iterate stage distributions: %w", err)
	}

	for i := range stages {
		accum := accums[stages[i].Stage]
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
