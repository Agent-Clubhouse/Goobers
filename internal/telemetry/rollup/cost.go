package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"sort"
	"time"
)

const (
	// CostExternalKindPR identifies pull-request attribution.
	CostExternalKindPR = "pr"
	// CostExternalKindIssue identifies issue attribution.
	CostExternalKindIssue = "issue"
)

// RunCostAttribution is one durable relationship between a run and an
// external issue or pull request.
type RunCostAttribution struct {
	RunID        string
	Provider     string
	ExternalKind string
	ExternalID   string
	Relationship string
}

// CostAggregate is exact usage attributed to one external issue or pull
// request. Nil measures are unmeasured; pointers to zero are measured zeroes.
type CostAggregate struct {
	Provider               string
	ExternalKind           string
	ExternalID             string
	TotalRuns              int
	MeasuredRuns           int
	TotalAttempts          int
	MeasuredAttempts       int
	InputTokens            *int64
	OutputTokens           *int64
	CacheReadTokens        *int64
	CacheWriteTokens       *int64
	ReasoningTokens        *int64
	CopilotPremiumRequests *float64
	NanoAIU                *int64
	CostUSD                *float64
	BillingModels          []string
	CostBases              []string
	Models                 []CostModelAggregate
}

// CostModelAggregate preserves the model dimension beneath an external cost
// aggregate.
type CostModelAggregate struct {
	Model                  string
	UsageAttempts          int
	MeasuredAttempts       int
	InputTokens            *int64
	OutputTokens           *int64
	CacheReadTokens        *int64
	CacheWriteTokens       *int64
	ReasoningTokens        *int64
	CopilotPremiumRequests *float64
	NanoAIU                *int64
	CostUSD                *float64
	BillingModels          []string
	CostBases              []string
}

type costMeasures struct {
	input, output, cacheRead, cacheWrite, reasoning optionalInt
	premium, costUSD                                optionalFloat
	nanoAIU                                         optionalInt
	billingModels, costBases                        map[string]struct{}
}

type optionalInt struct {
	value int64
	valid bool
}

type optionalFloat struct {
	value float64
	valid bool
}

type costRun struct {
	id               string
	started          time.Time
	issues           map[string]struct{}
	prs              map[string]struct{}
	measures         costMeasures
	attempts         int
	measuredAttempts int
	models           map[string]*costModelRun
}

type costModelRun struct {
	measures         costMeasures
	attempts         int
	measuredAttempts int
}

// RunCostAttributions returns a run's relationships in deterministic order.
func (db *DB) RunCostAttributions(ctx context.Context, runID string) ([]RunCostAttribution, error) {
	rows, err := db.readDB().QueryContext(ctx, `
		SELECT run_id, provider, external_kind, external_id, relationship
		FROM run_cost_attribution
		WHERE run_id = ?
		ORDER BY provider, external_kind, external_id, relationship`, runID)
	if err != nil {
		return nil, fmt.Errorf("rollup: query run cost attribution: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunCostAttribution
	for rows.Next() {
		var attribution RunCostAttribution
		if err := rows.Scan(
			&attribution.RunID,
			&attribution.Provider,
			&attribution.ExternalKind,
			&attribution.ExternalID,
			&attribution.Relationship,
		); err != nil {
			return nil, fmt.Errorf("rollup: scan run cost attribution: %w", err)
		}
		out = append(out, attribution)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rollup: iterate run cost attribution: %w", err)
	}
	return out, nil
}

// PullRequestCosts returns exact per-PR aggregates for provider. Every
// attributed attempt contributes, including failed attempts and retries.
func (db *DB) PullRequestCosts(ctx context.Context, provider string) ([]CostAggregate, error) {
	runs, err := db.loadCostRuns(ctx, provider)
	if err != nil {
		return nil, err
	}
	prIssues := addressedIssuesByPR(runs)
	prRuns := make(map[string]map[string]*costRun)
	for _, run := range runs {
		for pr := range run.prs {
			addCostRun(prRuns, pr, run)
		}
	}
	foldOrphanRuns(runs, prIssues, prRuns)
	return aggregateCostRuns(provider, CostExternalKindPR, prRuns), nil
}

// IssueCosts returns deterministic per-issue aggregates. Runs naming an issue
// are direct attribution. PR-only runs are split by known direct nano-AIU
// weights for the addressed issues, or evenly when no weights are known.
func (db *DB) IssueCosts(ctx context.Context, provider string) ([]CostAggregate, error) {
	runs, err := db.loadCostRuns(ctx, provider)
	if err != nil {
		return nil, err
	}
	prIssues := addressedIssuesByPR(runs)
	directWeights := make(map[string]int64)
	for _, run := range runs {
		if len(run.issues) == 0 || !run.measures.nanoAIU.valid {
			continue
		}
		ids := sortedSet(run.issues)
		for issue, share := range splitInt64(run.measures.nanoAIU.value, ids, nil) {
			directWeights[issue] += share
		}
	}

	aggregates := make(map[string]*CostAggregate)
	seenRuns := make(map[string]map[string]struct{})
	for _, run := range runs {
		targets := sortedSet(run.issues)
		if len(targets) == 0 {
			issueSet := make(map[string]struct{})
			for pr := range run.prs {
				for issue := range prIssues[pr] {
					issueSet[issue] = struct{}{}
				}
			}
			targets = sortedSet(issueSet)
		}
		if len(targets) == 0 {
			continue
		}
		weights := map[string]int64(nil)
		if len(run.issues) == 0 {
			weights = directWeights
		}
		shares := splitMeasures(run.measures, targets, weights)
		for _, issue := range targets {
			aggregate := aggregates[issue]
			if aggregate == nil {
				aggregate = &CostAggregate{
					Provider: provider, ExternalKind: CostExternalKindIssue, ExternalID: issue,
				}
				aggregates[issue] = aggregate
			}
			if seenRuns[issue] == nil {
				seenRuns[issue] = make(map[string]struct{})
			}
			if _, seen := seenRuns[issue][run.id]; !seen {
				seenRuns[issue][run.id] = struct{}{}
				aggregate.TotalRuns++
				aggregate.TotalAttempts += run.attempts
				if run.measures.measured() {
					aggregate.MeasuredRuns++
				}
				aggregate.MeasuredAttempts += run.measuredAttempts
			}
			addMeasuresToAggregate(aggregate, shares[issue])
			addModelsToIssueAggregate(aggregate, run, targets, weights, issue)
		}
	}
	return sortedAggregates(aggregates), nil
}

// CostTargetsForRun returns the pull requests touched by runID and every issue
// directly named by that run or addressed by one of those pull requests.
func (db *DB) CostTargetsForRun(ctx context.Context, provider, runID string) ([]string, []string, error) {
	runs, err := db.loadCostRuns(ctx, provider)
	if err != nil {
		return nil, nil, err
	}
	prIssues := addressedIssuesByPR(runs)
	prs := make(map[string]struct{})
	issues := make(map[string]struct{})
	for _, run := range runs {
		if run.id != runID {
			continue
		}
		for pr := range run.prs {
			prs[pr] = struct{}{}
			for issue := range prIssues[pr] {
				issues[issue] = struct{}{}
			}
		}
		for issue := range run.issues {
			issues[issue] = struct{}{}
		}
	}
	return sortedSet(prs), sortedSet(issues), nil
}

func (db *DB) loadCostRuns(ctx context.Context, provider string) ([]*costRun, error) {
	if provider == "" {
		return nil, fmt.Errorf("rollup: cost provider is required")
	}
	tx, err := db.readDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("rollup: begin cost query: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	byID, order, err := loadCostRunReferences(ctx, tx, provider)
	if err != nil {
		return nil, err
	}
	if err := loadCostAttemptUsage(ctx, tx, provider, byID); err != nil {
		return nil, err
	}
	if err := loadCostModelUsage(ctx, tx, provider, byID); err != nil {
		return nil, err
	}

	out := make([]*costRun, 0, len(order))
	for _, runID := range order {
		out = append(out, byID[runID])
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("rollup: commit cost query: %w", err)
	}
	return out, nil
}

func loadCostRunReferences(ctx context.Context, tx *sql.Tx, provider string) (map[string]*costRun, []string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT r.run_id, r.started_at, a.external_kind, a.external_id
		FROM runs r
		JOIN run_cost_attribution a ON a.run_id = r.run_id
		WHERE a.provider = ? AND a.external_kind IN ('pr', 'issue')
		GROUP BY r.run_id, r.started_at, a.external_kind, a.external_id
		ORDER BY r.started_at, r.run_id, a.external_kind, a.external_id`, provider)
	if err != nil {
		return nil, nil, fmt.Errorf("rollup: query cost-attributed runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[string]*costRun)
	var order []string
	for rows.Next() {
		var runID, startedText, kind, externalID string
		if err := rows.Scan(&runID, &startedText, &kind, &externalID); err != nil {
			return nil, nil, fmt.Errorf("rollup: scan cost-attributed run: %w", err)
		}
		run := byID[runID]
		if run == nil {
			started, err := time.Parse(time.RFC3339Nano, startedText)
			if err != nil {
				return nil, nil, fmt.Errorf("rollup: parse cost run start %q: %w", startedText, err)
			}
			run = &costRun{id: runID, started: started, issues: map[string]struct{}{}, prs: map[string]struct{}{}}
			byID[runID] = run
			order = append(order, runID)
		}
		switch kind {
		case CostExternalKindIssue:
			run.issues[externalID] = struct{}{}
		case CostExternalKindPR:
			run.prs[externalID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rollup: iterate cost-attributed runs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("rollup: close cost-attributed runs: %w", err)
	}
	return byID, order, nil
}

func loadCostAttemptUsage(ctx context.Context, tx *sql.Tx, provider string, byID map[string]*costRun) error {
	usageRows, err := tx.QueryContext(ctx, `
		SELECT sa.run_id, su.input_tokens, su.output_tokens,
		       su.cache_read_tokens, su.cache_write_tokens, su.reasoning_tokens,
		       su.copilot_premium_requests, su.nano_aiu, su.cost_usd,
		       su.billing_model, su.cost_basis
		FROM stage_attempts sa
		LEFT JOIN stage_usage su
			ON su.run_id = sa.run_id AND su.stage = sa.stage
			AND su.traversal = sa.traversal AND su.branch IS sa.branch
		WHERE EXISTS (
			SELECT 1 FROM run_cost_attribution a
			WHERE a.run_id = sa.run_id AND a.provider = ?
		)
		ORDER BY sa.run_id, sa.stage, sa.traversal`, provider)
	if err != nil {
		return fmt.Errorf("rollup: query attributed attempt usage: %w", err)
	}
	defer func() { _ = usageRows.Close() }()
	for usageRows.Next() {
		var runID string
		var input, output, cacheRead, cacheWrite, reasoning, nanoAIU sql.NullInt64
		var premium, costUSD sql.NullFloat64
		var billingModel, costBasis sql.NullString
		if err := usageRows.Scan(
			&runID, &input, &output, &cacheRead, &cacheWrite, &reasoning,
			&premium, &nanoAIU, &costUSD, &billingModel, &costBasis,
		); err != nil {
			return fmt.Errorf("rollup: scan attributed attempt usage: %w", err)
		}
		run := byID[runID]
		if run == nil {
			continue
		}
		run.attempts++
		if nanoAIU.Valid || costUSD.Valid {
			run.measuredAttempts++
		}
		run.measures.addRow(input, output, cacheRead, cacheWrite, reasoning, premium, nanoAIU, costUSD, billingModel, costBasis)
	}
	if err := usageRows.Err(); err != nil {
		return fmt.Errorf("rollup: iterate attributed attempt usage: %w", err)
	}
	if err := usageRows.Close(); err != nil {
		return fmt.Errorf("rollup: close attributed attempt usage: %w", err)
	}
	return nil
}

func loadCostModelUsage(ctx context.Context, tx *sql.Tx, provider string, byID map[string]*costRun) error {
	modelRows, err := tx.QueryContext(ctx, `
		SELECT smu.run_id, smu.model, smu.input_tokens, smu.output_tokens,
		       smu.cache_read_tokens, smu.cache_write_tokens, smu.reasoning_tokens,
		       smu.copilot_premium_requests, smu.nano_aiu, smu.cost_usd,
		       smu.billing_model, smu.cost_basis
		FROM stage_model_usage smu
		WHERE EXISTS (
			SELECT 1 FROM run_cost_attribution a
			WHERE a.run_id = smu.run_id AND a.provider = ?
		)
		ORDER BY smu.run_id, smu.stage, smu.traversal, smu.model`, provider)
	if err != nil {
		return fmt.Errorf("rollup: query attributed model usage: %w", err)
	}
	defer func() { _ = modelRows.Close() }()
	for modelRows.Next() {
		var runID, model string
		var input, output, cacheRead, cacheWrite, reasoning, nanoAIU sql.NullInt64
		var premium, costUSD sql.NullFloat64
		var billingModel, costBasis sql.NullString
		if err := modelRows.Scan(
			&runID, &model, &input, &output, &cacheRead, &cacheWrite, &reasoning,
			&premium, &nanoAIU, &costUSD, &billingModel, &costBasis,
		); err != nil {
			return fmt.Errorf("rollup: scan attributed model usage: %w", err)
		}
		run := byID[runID]
		if run == nil {
			continue
		}
		if run.models == nil {
			run.models = make(map[string]*costModelRun)
		}
		modelRun := run.models[model]
		if modelRun == nil {
			modelRun = &costModelRun{}
			run.models[model] = modelRun
		}
		modelRun.attempts++
		if nanoAIU.Valid || costUSD.Valid {
			modelRun.measuredAttempts++
		}
		modelRun.measures.addRow(input, output, cacheRead, cacheWrite, reasoning, premium, nanoAIU, costUSD, billingModel, costBasis)
	}
	if err := modelRows.Err(); err != nil {
		return fmt.Errorf("rollup: iterate attributed model usage: %w", err)
	}
	if err := modelRows.Close(); err != nil {
		return fmt.Errorf("rollup: close attributed model usage: %w", err)
	}
	return nil
}

func (m *costMeasures) addRow(input, output, cacheRead, cacheWrite, reasoning sql.NullInt64, premium sql.NullFloat64, nanoAIU sql.NullInt64, costUSD sql.NullFloat64, billingModel, costBasis sql.NullString) {
	addOptionalInt(&m.input, input)
	addOptionalInt(&m.output, output)
	addOptionalInt(&m.cacheRead, cacheRead)
	addOptionalInt(&m.cacheWrite, cacheWrite)
	addOptionalInt(&m.reasoning, reasoning)
	addOptionalFloat(&m.premium, premium)
	addOptionalInt(&m.nanoAIU, nanoAIU)
	addOptionalFloat(&m.costUSD, costUSD)
	if billingModel.Valid {
		if m.billingModels == nil {
			m.billingModels = make(map[string]struct{})
		}
		m.billingModels[billingModel.String] = struct{}{}
	}
	if costBasis.Valid {
		if m.costBases == nil {
			m.costBases = make(map[string]struct{})
		}
		m.costBases[costBasis.String] = struct{}{}
	}
}

func (m costMeasures) measured() bool {
	return m.nanoAIU.valid || m.costUSD.valid
}

func addOptionalInt(dst *optionalInt, value sql.NullInt64) {
	if value.Valid {
		dst.value += value.Int64
		dst.valid = true
	}
}

func addOptionalFloat(dst *optionalFloat, value sql.NullFloat64) {
	if value.Valid {
		dst.value += value.Float64
		dst.valid = true
	}
}

func addressedIssuesByPR(runs []*costRun) map[string]map[string]struct{} {
	out := make(map[string]map[string]struct{})
	for _, run := range runs {
		for pr := range run.prs {
			if out[pr] == nil {
				out[pr] = make(map[string]struct{})
			}
			for issue := range run.issues {
				out[pr][issue] = struct{}{}
			}
		}
	}
	return out
}

func foldOrphanRuns(runs []*costRun, prIssues map[string]map[string]struct{}, prRuns map[string]map[string]*costRun) {
	type candidate struct {
		id      string
		started time.Time
	}
	var candidates []candidate
	for pr, assigned := range prRuns {
		var first time.Time
		for _, run := range assigned {
			if first.IsZero() || run.started.Before(first) {
				first = run.started
			}
		}
		candidates = append(candidates, candidate{id: pr, started: first})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].started.Equal(candidates[j].started) {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].started.Before(candidates[j].started)
	})
	for _, run := range runs {
		if len(run.prs) != 0 || len(run.issues) == 0 {
			continue
		}
		best := -1
		for i, pr := range candidates {
			if !setsIntersect(run.issues, prIssues[pr.id]) {
				continue
			}
			if best == -1 {
				best = i
				continue
			}
			bestIsBefore := candidates[best].started.Before(run.started)
			currentIsBefore := pr.started.Before(run.started)
			if bestIsBefore != currentIsBefore {
				if !currentIsBefore {
					best = i
				}
				continue
			}
			if currentIsBefore {
				if pr.started.After(candidates[best].started) {
					best = i
				}
			} else if pr.started.Before(candidates[best].started) {
				best = i
			}
		}
		if best >= 0 {
			addCostRun(prRuns, candidates[best].id, run)
		}
	}
}

func aggregateCostRuns(provider, kind string, groups map[string]map[string]*costRun) []CostAggregate {
	out := make(map[string]*CostAggregate, len(groups))
	for externalID, runs := range groups {
		aggregate := &CostAggregate{Provider: provider, ExternalKind: kind, ExternalID: externalID}
		runIDs := make([]string, 0, len(runs))
		for runID := range runs {
			runIDs = append(runIDs, runID)
		}
		sort.Strings(runIDs)
		for _, runID := range runIDs {
			run := runs[runID]
			aggregate.TotalRuns++
			aggregate.TotalAttempts += run.attempts
			if run.measures.measured() {
				aggregate.MeasuredRuns++
			}
			aggregate.MeasuredAttempts += run.measuredAttempts
			addMeasuresToAggregate(aggregate, run.measures)
			addModelsToAggregate(aggregate, run)
		}
		out[externalID] = aggregate
	}
	return sortedAggregates(out)
}

func addCostRun(groups map[string]map[string]*costRun, externalID string, run *costRun) {
	if groups[externalID] == nil {
		groups[externalID] = make(map[string]*costRun)
	}
	groups[externalID][run.id] = run
}

func addMeasuresToAggregate(dst *CostAggregate, src costMeasures) {
	addIntPointer(&dst.InputTokens, src.input)
	addIntPointer(&dst.OutputTokens, src.output)
	addIntPointer(&dst.CacheReadTokens, src.cacheRead)
	addIntPointer(&dst.CacheWriteTokens, src.cacheWrite)
	addIntPointer(&dst.ReasoningTokens, src.reasoning)
	addFloatPointer(&dst.CopilotPremiumRequests, src.premium)
	addIntPointer(&dst.NanoAIU, src.nanoAIU)
	addFloatPointer(&dst.CostUSD, src.costUSD)
	dst.BillingModels = mergeNames(dst.BillingModels, src.billingModels)
	dst.CostBases = mergeNames(dst.CostBases, src.costBases)
}

func addModelsToAggregate(dst *CostAggregate, run *costRun) {
	for model, source := range run.models {
		target := modelAggregate(dst, model)
		target.UsageAttempts += source.attempts
		target.MeasuredAttempts += source.measuredAttempts
		addMeasuresToModelAggregate(target, source.measures)
	}
	sort.Slice(dst.Models, func(i, j int) bool { return dst.Models[i].Model < dst.Models[j].Model })
}

func addModelsToIssueAggregate(dst *CostAggregate, run *costRun, targets []string, weights map[string]int64, issue string) {
	for model, source := range run.models {
		shares := splitMeasures(source.measures, targets, weights)
		target := modelAggregate(dst, model)
		target.UsageAttempts += source.attempts
		target.MeasuredAttempts += source.measuredAttempts
		addMeasuresToModelAggregate(target, shares[issue])
	}
	sort.Slice(dst.Models, func(i, j int) bool { return dst.Models[i].Model < dst.Models[j].Model })
}

func modelAggregate(dst *CostAggregate, model string) *CostModelAggregate {
	for i := range dst.Models {
		if dst.Models[i].Model == model {
			return &dst.Models[i]
		}
	}
	dst.Models = append(dst.Models, CostModelAggregate{Model: model})
	return &dst.Models[len(dst.Models)-1]
}

func addMeasuresToModelAggregate(dst *CostModelAggregate, src costMeasures) {
	addIntPointer(&dst.InputTokens, src.input)
	addIntPointer(&dst.OutputTokens, src.output)
	addIntPointer(&dst.CacheReadTokens, src.cacheRead)
	addIntPointer(&dst.CacheWriteTokens, src.cacheWrite)
	addIntPointer(&dst.ReasoningTokens, src.reasoning)
	addFloatPointer(&dst.CopilotPremiumRequests, src.premium)
	addIntPointer(&dst.NanoAIU, src.nanoAIU)
	addFloatPointer(&dst.CostUSD, src.costUSD)
	dst.BillingModels = mergeNames(dst.BillingModels, src.billingModels)
	dst.CostBases = mergeNames(dst.CostBases, src.costBases)
}

func addIntPointer(dst **int64, src optionalInt) {
	if !src.valid {
		return
	}
	if *dst == nil {
		*dst = new(int64)
	}
	**dst += src.value
}

func addFloatPointer(dst **float64, src optionalFloat) {
	if !src.valid {
		return
	}
	if *dst == nil {
		*dst = new(float64)
	}
	**dst += src.value
}

func splitMeasures(measures costMeasures, targets []string, weights map[string]int64) map[string]costMeasures {
	out := make(map[string]costMeasures, len(targets))
	ints := []struct {
		value optionalInt
		set   func(*costMeasures, optionalInt)
	}{
		{measures.input, func(m *costMeasures, v optionalInt) { m.input = v }},
		{measures.output, func(m *costMeasures, v optionalInt) { m.output = v }},
		{measures.cacheRead, func(m *costMeasures, v optionalInt) { m.cacheRead = v }},
		{measures.cacheWrite, func(m *costMeasures, v optionalInt) { m.cacheWrite = v }},
		{measures.reasoning, func(m *costMeasures, v optionalInt) { m.reasoning = v }},
		{measures.nanoAIU, func(m *costMeasures, v optionalInt) { m.nanoAIU = v }},
	}
	for _, field := range ints {
		if !field.value.valid {
			continue
		}
		for target, value := range splitInt64(field.value.value, targets, weights) {
			m := out[target]
			field.set(&m, optionalInt{value: value, valid: true})
			out[target] = m
		}
	}
	floats := []struct {
		value optionalFloat
		set   func(*costMeasures, optionalFloat)
	}{
		{measures.premium, func(m *costMeasures, v optionalFloat) { m.premium = v }},
		{measures.costUSD, func(m *costMeasures, v optionalFloat) { m.costUSD = v }},
	}
	for _, field := range floats {
		if !field.value.valid {
			continue
		}
		totalWeight := weightTotal(targets, weights)
		for _, target := range targets {
			weight := targetWeight(target, targets, weights)
			m := out[target]
			field.set(&m, optionalFloat{value: field.value.value * float64(weight) / float64(totalWeight), valid: true})
			out[target] = m
		}
	}
	for _, target := range targets {
		m := out[target]
		m.billingModels = cloneSet(measures.billingModels)
		m.costBases = cloneSet(measures.costBases)
		out[target] = m
	}
	return out
}

func splitInt64(value int64, targets []string, weights map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(targets))
	totalWeight := weightTotal(targets, weights)
	type remainder struct {
		target string
		value  *big.Int
	}
	remainders := make([]remainder, 0, len(targets))
	var assigned int64
	divisor := big.NewInt(totalWeight)
	for _, target := range targets {
		product := new(big.Int).Mul(big.NewInt(value), big.NewInt(targetWeight(target, targets, weights)))
		quotient, rem := new(big.Int), new(big.Int)
		quotient.QuoRem(product, divisor, rem)
		share := quotient.Int64()
		out[target] = share
		assigned += share
		remainders = append(remainders, remainder{target: target, value: rem})
	}
	sort.SliceStable(remainders, func(i, j int) bool {
		if cmp := remainders[i].value.Cmp(remainders[j].value); cmp != 0 {
			return cmp > 0
		}
		return remainders[i].target < remainders[j].target
	})
	for i := int64(0); i < value-assigned; i++ {
		out[remainders[i%int64(len(remainders))].target]++
	}
	return out
}

func weightTotal(targets []string, weights map[string]int64) int64 {
	var total int64
	for _, target := range targets {
		if weights[target] > 0 {
			total += weights[target]
		}
	}
	if total == 0 {
		return int64(len(targets))
	}
	return total
}

func targetWeight(target string, targets []string, weights map[string]int64) int64 {
	if weight := weights[target]; weight > 0 {
		return weight
	}
	for _, candidate := range targets {
		if weights[candidate] > 0 {
			return 0
		}
	}
	return 1
}

func sortedSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func setsIntersect(left, right map[string]struct{}) bool {
	for value := range left {
		if _, ok := right[value]; ok {
			return true
		}
	}
	return false
}

func sortedAggregates(values map[string]*CostAggregate) []CostAggregate {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]CostAggregate, 0, len(keys))
	for _, key := range keys {
		out = append(out, *values[key])
	}
	return out
}

func mergeNames(existing []string, additions map[string]struct{}) []string {
	set := make(map[string]struct{}, len(existing)+len(additions))
	for _, value := range existing {
		set[value] = struct{}{}
	}
	for value := range additions {
		set[value] = struct{}{}
	}
	return sortedSet(set)
}

func cloneSet(values map[string]struct{}) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for value := range values {
		out[value] = struct{}{}
	}
	return out
}
