package rollup

import (
	"context"
	"database/sql"
	"fmt"
)

// TrendStats aggregates all requested windows with one database query.
func (db *DB) TrendStats(ctx context.Context, req TrendRequest) ([]TrendResult, error) {
	if len(req.Windows) == 0 {
		return nil, nil
	}
	if err := db.requireKnownBranchAttribution(ctx, req.Stats); err != nil {
		return nil, err
	}

	clauses, args := statsClauses("r.workflow", "r.gaggle", "r.started_at", req.Stats)
	branchClauses, branchArgs := branchFilterClauses("sa", req.Stats)
	clauses = append(clauses, branchClauses...)
	args = append(args, branchArgs...)
	join := ""
	if agentStatsActive(req.Stats) {
		join = `JOIN agent_invocations ai
			ON ai.run_id = sa.run_id AND ai.stage = sa.stage AND ai.traversal = sa.traversal
			AND ai.kind = 'task'`
		agentClauses, agentArgs := agentFilterClauses("ai", req.Stats)
		clauses = append(clauses, agentClauses...)
		args = append(args, agentArgs...)
	}
	bucketExpr := "CASE"
	bucketArgs := make([]any, 0, len(req.Windows)*3)
	for index, window := range req.Windows {
		bucketExpr += " WHEN r.started_at >= ? AND r.started_at < ? THEN ?"
		bucketArgs = append(bucketArgs, formatTime(window.Since).String, formatTime(window.Until).String, index)
	}
	bucketExpr += " END"
	clauses = append(clauses, bucketExpr+" IS NOT NULL")
	dimensions := stageDimensionColumns(req.Stats, "sa", "ai")
	query := fmt.Sprintf(`
		SELECT %s, r.gaggle, r.workflow, sa.stage%s,
		       sa.duration_ms, su.input_tokens, su.output_tokens,
		       su.copilot_premium_requests, su.cost_usd
		FROM stage_attempts sa
		JOIN runs r ON r.run_id = sa.run_id
		%s
		LEFT JOIN stage_usage su
			ON su.run_id = sa.run_id AND su.stage = sa.stage
			AND su.traversal = sa.traversal AND su.branch IS sa.branch
		JOIN (
			SELECT run_id, stage, branch, MAX(traversal) AS final_traversal
			FROM stage_attempts GROUP BY run_id, stage, branch
		) latest ON latest.run_id = sa.run_id AND latest.stage = sa.stage
			AND latest.branch IS sa.branch
			AND latest.final_traversal = sa.traversal
		%s
		%s
		ORDER BY %s, r.gaggle, r.workflow, sa.stage%s, sa.run_id, sa.traversal`,
		bucketExpr, prefixedColumns(dimensions), join, whereClause(clauses),
		"", "1", groupedColumns(dimensions))
	queryArgs := make([]any, 0, len(bucketArgs)*2+len(args))
	queryArgs = append(queryArgs, bucketArgs...)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, bucketArgs...)
	rows, err := db.readDB().QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query trend usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	accums := make([]map[stageDistributionKey]*stageDistributionAccum, len(req.Windows))
	for i := range accums {
		accums[i] = make(map[stageDistributionKey]*stageDistributionAccum)
	}
	for rows.Next() {
		var bucket int
		var key stageDistributionKey
		var duration, input, output sql.NullInt64
		var premium, cost sql.NullFloat64
		var branch sql.NullInt64
		scan := []any{&bucket, &key.gaggle, &key.workflow, &key.stage}
		scan = appendStageDimensionScan(scan, req.Stats, &branch, &key.model, &key.harnessVersion)
		scan = append(scan, &duration, &input, &output, &premium, &cost)
		if err := rows.Scan(scan...); err != nil {
			return nil, fmt.Errorf("rollup: scan trend usage: %w", err)
		}
		if bucket < 0 || bucket >= len(accums) {
			continue
		}
		if req.Stats.GroupByBranch && branch.Valid {
			key.branch, key.branchKnown = int(branch.Int64), true
		} else if req.Stats.Branch != nil {
			key.branch, key.branchKnown = *req.Stats.Branch, true
		}
		accum := accums[bucket][key]
		if accum == nil {
			accum = &stageDistributionAccum{}
			accums[bucket][key] = accum
		}
		accum.attempts++
		if duration.Valid && duration.Int64 >= 0 {
			accum.durations = append(accum.durations, duration.Int64)
		}
		if input.Valid && output.Valid {
			if input.Int64 < 0 || output.Int64 < 0 {
				return nil, fmt.Errorf("rollup: stage %s has negative token usage", key.stage)
			}
			tokens, err := addNonnegativeInt64(input.Int64, output.Int64)
			if err != nil {
				return nil, fmt.Errorf("rollup: sum token usage for stage %s: %w", key.stage, err)
			}
			accum.tokens = append(accum.tokens, tokens)
		}
		if premium.Valid {
			accum.premiumRequests = append(accum.premiumRequests, premium.Float64)
		}
		if cost.Valid {
			accum.costs = append(accum.costs, cost.Float64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rollup: iterate trend usage: %w", err)
	}
	results := make([]TrendResult, len(req.Windows))
	for i := range accums {
		usage, err := usageStats(accums[i], req.Stats.GroupByBranch || req.Stats.Branch != nil)
		if err != nil {
			return nil, err
		}
		results[i].Usage = usage
	}
	return results, nil
}
