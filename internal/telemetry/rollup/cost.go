package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/intsplit"
)

const (
	// CostExternalKindPR identifies pull-request attribution.
	CostExternalKindPR = "pr"
	// CostExternalKindIssue identifies issue attribution.
	CostExternalKindIssue = "issue"
)

// CostQuery selects bounded cost aggregates. An empty ExternalKind returns
// both pull-request and issue aggregates; an empty Provider returns every
// provider represented in the selected window.
type CostQuery struct {
	Provider     string
	ExternalKind string
	ExternalID   string
	Gaggle       string
	Workflow     string
	Stage        string
	Since        time.Time
	Until        time.Time
}

// CostResult contains deterministic per-provider aggregates.
type CostResult struct {
	PullRequests []CostAggregate
	Issues       []CostAggregate
}

// RunCostAttribution is one durable relationship between a run and an
// external issue or pull request.
type RunCostAttribution struct {
	RunID        string
	Provider     string
	Repository   string
	ExternalKind string
	ExternalID   string
	URL          string
	Relationship string
}

// CostAggregate is exact usage attributed to one external issue or pull
// request. Nil measures are unmeasured; pointers to zero are measured zeroes.
type CostAggregate struct {
	Provider               string
	Repository             string
	URL                    string
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
	Runs                   []CostRunAggregate
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

// CostRunAggregate preserves the run dimension beneath an external cost
// aggregate.
type CostRunAggregate struct {
	RunID                  string
	Gaggle                 string
	Workflow               string
	Status                 string
	StartedAt              time.Time
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
	Models                 []CostModelAggregate
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
	gaggle           string
	workflow         string
	status           string
	started          time.Time
	issues           map[string]string
	prs              map[string]string
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

type attributedCostUsageSpec struct {
	table, alias, selectColumns, joins, orderBy string
	stageColumn, errorName                      string
	hasModel                                    bool
	add                                         func(runID string, model *string, measures costMeasures)
}

type costMeasureDestination struct {
	input, output, cacheRead, cacheWrite, reasoning **int64
	premium, costUSD                                **float64
	nanoAIU                                         **int64
	billingModels, costBases                        *[]string
}

// RunCostAttributions returns a run's relationships in deterministic order.
func (db *DB) RunCostAttributions(ctx context.Context, runID string) ([]RunCostAttribution, error) {
	rows, err := db.readDB().QueryContext(ctx, `
		SELECT run_id, provider, repository, external_kind, external_id, COALESCE(url, ''), relationship
		FROM run_cost_attribution
		WHERE run_id = ?
		ORDER BY provider, repository, external_kind, external_id, relationship`, runID)
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
			&attribution.Repository,
			&attribution.ExternalKind,
			&attribution.ExternalID,
			&attribution.URL,
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
	result, err := db.CostAggregates(ctx, CostQuery{
		Provider:     provider,
		ExternalKind: CostExternalKindPR,
	})
	if err != nil {
		return nil, err
	}
	return result.PullRequests, nil
}

// IssueCosts returns deterministic per-issue aggregates. Runs naming an issue
// are direct attribution. PR-only runs are split by known direct nano-AIU
// weights for the addressed issues, or evenly when no weights are known.
func (db *DB) IssueCosts(ctx context.Context, provider string) ([]CostAggregate, error) {
	result, err := db.CostAggregates(ctx, CostQuery{
		Provider:     provider,
		ExternalKind: CostExternalKindIssue,
	})
	if err != nil {
		return nil, err
	}
	return result.Issues, nil
}

// CostAggregates returns bounded pull-request and/or issue aggregates through
// one typed store boundary.
func (db *DB) CostAggregates(ctx context.Context, query CostQuery) (CostResult, error) {
	if query.ExternalKind != "" &&
		query.ExternalKind != CostExternalKindPR &&
		query.ExternalKind != CostExternalKindIssue {
		return CostResult{}, fmt.Errorf("rollup: unsupported cost external kind %q", query.ExternalKind)
	}
	if !query.Since.IsZero() && !query.Until.IsZero() && !query.Since.Before(query.Until) {
		return CostResult{}, fmt.Errorf("rollup: cost since must be before until")
	}
	providers, err := db.costProviders(ctx, query)
	if err != nil {
		return CostResult{}, err
	}
	result := CostResult{
		PullRequests: []CostAggregate{},
		Issues:       []CostAggregate{},
	}
	for _, provider := range providers {
		runs, err := db.loadCostRuns(ctx, provider, query)
		if err != nil {
			return CostResult{}, err
		}
		if query.ExternalKind == "" || query.ExternalKind == CostExternalKindPR {
			aggregates := filterCostAggregates(pullRequestCostAggregates(provider, runs), query.ExternalID)
			if err := db.enrichCostWorkItemIdentities(ctx, provider, aggregates); err != nil {
				return CostResult{}, err
			}
			result.PullRequests = append(result.PullRequests, aggregates...)
		}
		if query.ExternalKind == "" || query.ExternalKind == CostExternalKindIssue {
			aggregates := filterCostAggregates(issueCostAggregates(provider, runs), query.ExternalID)
			if err := db.enrichCostWorkItemIdentities(ctx, provider, aggregates); err != nil {
				return CostResult{}, err
			}
			result.Issues = append(result.Issues, aggregates...)
		}
	}
	return result, nil
}

func (db *DB) enrichCostWorkItemIdentities(ctx context.Context, provider string, aggregates []CostAggregate) error {
	if len(aggregates) == 0 {
		return nil
	}
	rows, err := db.readDB().QueryContext(ctx, `
		WITH normalized AS (
			SELECT
				kind,
				external_id,
				CASE
					WHEN instr(COALESCE(url, ''), '#') > 0
						THEN substr(url, 1, instr(url, '#') - 1)
					ELSE COALESCE(url, '')
				END AS canonical_url
			FROM provider_mutations
			WHERE provider = ? AND kind IN ('pr', 'issue')
		)
		SELECT kind, external_id,
		       CASE
			       WHEN COUNT(DISTINCT lower(canonical_url)) = 1 THEN MAX(canonical_url)
			       ELSE ''
		       END
		FROM normalized
		WHERE canonical_url <> ''
		GROUP BY kind, external_id`, provider)
	if err != nil {
		return fmt.Errorf("rollup: query cost work item identities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	identities := make(map[string]string)
	for rows.Next() {
		var kind, externalID, itemURL string
		if err := rows.Scan(&kind, &externalID, &itemURL); err != nil {
			return fmt.Errorf("rollup: scan cost work item identity: %w", err)
		}
		identities[kind+"\x00"+externalID] = itemURL
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rollup: iterate cost work item identities: %w", err)
	}
	for index := range aggregates {
		if aggregates[index].Repository != "" {
			continue
		}
		itemURL := identities[aggregates[index].ExternalKind+"\x00"+aggregates[index].ExternalID]
		aggregates[index].URL = itemURL
		aggregates[index].Repository = workItemRepository(provider, itemURL)
	}
	return nil
}

func (db *DB) costProviders(ctx context.Context, costQuery CostQuery) ([]string, error) {
	if costQuery.Provider != "" {
		return []string{costQuery.Provider}, nil
	}
	query := `
		SELECT DISTINCT a.provider
		FROM run_cost_attribution a
		JOIN runs r ON r.run_id = a.run_id
		WHERE a.provider <> ''`
	var args []any
	query, args = appendCostRunScope(query, args, "r", costQuery)
	query += ` ORDER BY a.provider`
	rows, err := db.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query cost providers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var providers []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("rollup: scan cost provider: %w", err)
		}
		providers = append(providers, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rollup: iterate cost providers: %w", err)
	}
	return providers, nil
}

func pullRequestCostAggregates(provider string, runs []*costRun) []CostAggregate {
	prIssues := addressedIssuesByPR(runs)
	prRuns := make(map[string]map[string]*costRun)
	for _, run := range runs {
		for pr := range run.prs {
			addCostRun(prRuns, pr, run)
		}
	}
	foldOrphanRuns(runs, prIssues, prRuns)
	return aggregateCostRuns(provider, CostExternalKindPR, prRuns)
}

func issueCostAggregates(provider string, runs []*costRun) []CostAggregate {
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
		for _, identity := range targets {
			repository, externalID := costReferenceParts(identity)
			aggregate := aggregates[identity]
			if aggregate == nil {
				aggregate = &CostAggregate{
					Provider: provider, Repository: repository,
					ExternalKind: CostExternalKindIssue, ExternalID: externalID,
				}
				aggregates[identity] = aggregate
			}
			if aggregate.URL == "" {
				aggregate.URL = run.issues[identity]
			}
			if seenRuns[identity] == nil {
				seenRuns[identity] = make(map[string]struct{})
			}
			if _, seen := seenRuns[identity][run.id]; !seen {
				seenRuns[identity][run.id] = struct{}{}
				aggregate.TotalRuns++
				aggregate.TotalAttempts += run.attempts
				if run.measures.measured() {
					aggregate.MeasuredRuns++
				}
				aggregate.MeasuredAttempts += run.measuredAttempts
			}
			addMeasuresToAggregate(aggregate, shares[identity])
			addModelsToIssueAggregate(aggregate, run, targets, weights, identity)
			aggregate.Runs = append(aggregate.Runs, issueCostRunAggregate(run, shares[identity], targets, weights, identity))
		}
	}
	for _, aggregate := range aggregates {
		sortCostRuns(aggregate.Runs)
	}
	return sortedAggregates(aggregates)
}

func filterCostAggregates(aggregates []CostAggregate, externalID string) []CostAggregate {
	if externalID == "" {
		return aggregates
	}
	var filtered []CostAggregate
	for _, aggregate := range aggregates {
		if aggregate.ExternalID == externalID {
			filtered = append(filtered, aggregate)
		}
	}
	return filtered
}

func (db *DB) loadCostRuns(ctx context.Context, provider string, query CostQuery) ([]*costRun, error) {
	if provider == "" {
		return nil, fmt.Errorf("rollup: cost provider is required")
	}
	tx, err := db.readDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("rollup: begin cost query: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	byID, order, err := loadCostRunReferences(ctx, tx, provider, query)
	if err != nil {
		return nil, err
	}
	if err := loadCostAttemptUsage(ctx, tx, provider, query, byID); err != nil {
		return nil, err
	}
	if err := loadCostModelUsage(ctx, tx, provider, query, byID); err != nil {
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

func loadCostRunReferences(ctx context.Context, tx *sql.Tx, provider string, costQuery CostQuery) (map[string]*costRun, []string, error) {
	query := `
		SELECT r.run_id, r.gaggle, r.workflow, COALESCE(r.status, ''), r.started_at, a.repository, a.external_kind, a.external_id, COALESCE(a.url, '')
		FROM runs r
		JOIN run_cost_attribution a ON a.run_id = r.run_id
		WHERE a.provider = ? AND a.external_kind IN ('pr', 'issue')`
	args := []any{provider}
	query, args = appendCostRunScope(query, args, "r", costQuery)
	query += `
		GROUP BY r.run_id, r.gaggle, r.workflow, r.status, r.started_at, a.repository, a.external_kind, a.external_id, a.url
		ORDER BY r.started_at, r.run_id, a.external_kind, a.repository, a.external_id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("rollup: query cost-attributed runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[string]*costRun)
	var order []string
	for rows.Next() {
		var runID, gaggle, workflow, status, startedText, repository, kind, externalID, itemURL string
		if err := rows.Scan(&runID, &gaggle, &workflow, &status, &startedText, &repository, &kind, &externalID, &itemURL); err != nil {
			return nil, nil, fmt.Errorf("rollup: scan cost-attributed run: %w", err)
		}
		run := byID[runID]
		if run == nil {
			started, err := time.Parse(time.RFC3339Nano, startedText)
			if err != nil {
				return nil, nil, fmt.Errorf("rollup: parse cost run start %q: %w", startedText, err)
			}
			run = &costRun{id: runID, gaggle: gaggle, workflow: workflow, status: status, started: started, issues: map[string]string{}, prs: map[string]string{}}
			byID[runID] = run
			order = append(order, runID)
		}
		identity := costReferenceKey(repository, externalID)
		switch kind {
		case CostExternalKindIssue:
			run.issues[identity] = itemURL
		case CostExternalKindPR:
			run.prs[identity] = itemURL
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rollup: iterate cost-attributed runs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("rollup: close cost-attributed runs: %w", err)
	}
	for _, run := range byID {
		foldUnqualifiedCostReferences(run.issues)
		foldUnqualifiedCostReferences(run.prs)
	}
	return byID, order, nil
}

// foldUnqualifiedCostReferences merges a run's repository-less reference into
// the run's single repository-qualified reference to the same external id.
//
// #6796: ADO claim and claim-release receipts were recorded without a URL, so
// already-ingested rows carry an empty repository beside the qualified rows the
// same run wrote for the same work item. Kept apart, they split one item into
// two aggregates at half cost each. The fold is per run and only when exactly
// one qualified identity exists for the id; with zero or several candidates the
// unqualified reference stays as it is, because an identity the evidence does
// not determine must stay unknown rather than be guessed (#5266).
func foldUnqualifiedCostReferences(refs map[string]string) {
	qualified := make(map[string][]string)
	for identity := range refs {
		repository, externalID := costReferenceParts(identity)
		if repository != "" {
			qualified[externalID] = append(qualified[externalID], identity)
		}
	}
	for identity := range refs {
		repository, externalID := costReferenceParts(identity)
		if repository != "" || len(qualified[externalID]) != 1 {
			continue
		}
		target := qualified[externalID][0]
		if refs[target] == "" {
			refs[target] = refs[identity]
		}
		delete(refs, identity)
	}
}

func loadCostAttemptUsage(ctx context.Context, tx *sql.Tx, provider string, costQuery CostQuery, byID map[string]*costRun) error {
	return loadAttributedCostUsage(ctx, tx, provider, costQuery, attributedCostUsageSpec{
		table:         "stage_attempts",
		alias:         "sa",
		selectColumns: "su.input_tokens, su.output_tokens, su.cache_read_tokens, su.cache_write_tokens, su.reasoning_tokens, su.copilot_premium_requests, su.nano_aiu, su.cost_usd, su.billing_model, su.cost_basis",
		joins: `LEFT JOIN stage_usage su
			ON su.run_id = sa.run_id AND su.stage = sa.stage
			AND su.traversal = sa.traversal AND su.branch IS sa.branch`,
		stageColumn: "sa.stage",
		orderBy:     "sa.run_id, sa.stage, sa.traversal",
		errorName:   "attempt",
		add: func(runID string, _ *string, measures costMeasures) {
			run := byID[runID]
			if run == nil {
				return
			}
			run.attempts++
			if measures.measured() {
				run.measuredAttempts++
			}
			run.measures.add(measures)
		},
	})
}

func loadCostModelUsage(ctx context.Context, tx *sql.Tx, provider string, costQuery CostQuery, byID map[string]*costRun) error {
	return loadAttributedCostUsage(ctx, tx, provider, costQuery, attributedCostUsageSpec{
		table:         "stage_model_usage",
		alias:         "smu",
		selectColumns: "smu.model, smu.input_tokens, smu.output_tokens, smu.cache_read_tokens, smu.cache_write_tokens, smu.reasoning_tokens, smu.copilot_premium_requests, smu.nano_aiu, smu.cost_usd, smu.billing_model, smu.cost_basis",
		stageColumn:   "smu.stage",
		orderBy:       "smu.run_id, smu.stage, smu.traversal, smu.model",
		errorName:     "model",
		hasModel:      true,
		add: func(runID string, model *string, measures costMeasures) {
			run := byID[runID]
			if run == nil {
				return
			}
			if run.models == nil {
				run.models = make(map[string]*costModelRun)
			}
			modelRun := run.models[*model]
			if modelRun == nil {
				modelRun = &costModelRun{}
				run.models[*model] = modelRun
			}
			modelRun.attempts++
			if measures.measured() {
				modelRun.measuredAttempts++
			}
			modelRun.measures.add(measures)
		},
	})
}

func loadAttributedCostUsage(ctx context.Context, tx *sql.Tx, provider string, costQuery CostQuery, spec attributedCostUsageSpec) error {
	query := "\n\t\tSELECT " + spec.alias + ".run_id, " + spec.selectColumns +
		"\n\t\tFROM " + spec.table + " " + spec.alias +
		"\n\t\tJOIN runs r ON r.run_id = " + spec.alias + ".run_id"
	if spec.joins != "" {
		query += "\n\t\t" + spec.joins
	}
	query += `
		WHERE EXISTS (
			SELECT 1 FROM run_cost_attribution a
			WHERE a.run_id = ` + spec.alias + `.run_id AND a.provider = ?
		)`
	args := []any{provider}
	query, args = appendCostRunIdentityScope(query, args, "r", costQuery)
	if costQuery.Stage != "" {
		query += " AND " + spec.stageColumn + " = ?"
		args = append(args, costQuery.Stage)
	}
	query, args = appendCostWindow(query, args, "r.started_at", costQuery.Since, costQuery.Until)
	query += " ORDER BY " + spec.orderBy
	usageRows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("rollup: query attributed %s usage: %w", spec.errorName, err)
	}
	defer func() { _ = usageRows.Close() }()
	for usageRows.Next() {
		runID, model, measures, err := scanAttributedCostUsage(usageRows, spec.hasModel)
		if err != nil {
			return fmt.Errorf("rollup: scan attributed %s usage: %w", spec.errorName, err)
		}
		spec.add(runID, model, measures)
	}
	if err := usageRows.Err(); err != nil {
		return fmt.Errorf("rollup: iterate attributed %s usage: %w", spec.errorName, err)
	}
	if err := usageRows.Close(); err != nil {
		return fmt.Errorf("rollup: close attributed %s usage: %w", spec.errorName, err)
	}
	return nil
}

func scanAttributedCostUsage(rows *sql.Rows, hasModel bool) (string, *string, costMeasures, error) {
	var runID, model string
	var input, output, cacheRead, cacheWrite, reasoning, nanoAIU sql.NullInt64
	var premium, costUSD sql.NullFloat64
	var billingModel, costBasis sql.NullString
	destinations := []any{&runID}
	if hasModel {
		destinations = append(destinations, &model)
	}
	destinations = append(destinations,
		&input, &output, &cacheRead, &cacheWrite, &reasoning,
		&premium, &nanoAIU, &costUSD, &billingModel, &costBasis,
	)
	if err := rows.Scan(destinations...); err != nil {
		return "", nil, costMeasures{}, err
	}
	measures := costMeasures{}
	measures.addRow(input, output, cacheRead, cacheWrite, reasoning, premium, nanoAIU, costUSD, billingModel, costBasis)
	if hasModel {
		return runID, &model, measures, nil
	}
	return runID, nil, measures, nil
}

func appendCostWindow(query string, args []any, column string, since, until time.Time) (string, []any) {
	if !since.IsZero() {
		query += " AND " + column + " >= ?"
		args = append(args, formatTime(since).String)
	}
	if !until.IsZero() {
		query += " AND " + column + " < ?"
		args = append(args, formatTime(until).String)
	}
	return query, args
}

func appendCostRunScope(query string, args []any, runAlias string, costQuery CostQuery) (string, []any) {
	query, args = appendCostRunIdentityScope(query, args, runAlias, costQuery)
	if costQuery.Stage != "" {
		query += " AND EXISTS (SELECT 1 FROM stage_attempts scoped_sa WHERE scoped_sa.run_id = " +
			runAlias + ".run_id AND scoped_sa.stage = ?)"
		args = append(args, costQuery.Stage)
	}
	return appendCostWindow(query, args, runAlias+".started_at", costQuery.Since, costQuery.Until)
}

func appendCostRunIdentityScope(query string, args []any, runAlias string, costQuery CostQuery) (string, []any) {
	if costQuery.Gaggle != "" {
		query += " AND " + runAlias + ".gaggle = ?"
		args = append(args, costQuery.Gaggle)
	}
	if costQuery.Workflow != "" {
		query += " AND " + runAlias + ".workflow = ?"
		args = append(args, costQuery.Workflow)
	}
	return query, args
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

func (m *costMeasures) add(other costMeasures) {
	m.input.value += other.input.value
	m.input.valid = m.input.valid || other.input.valid
	m.output.value += other.output.value
	m.output.valid = m.output.valid || other.output.valid
	m.cacheRead.value += other.cacheRead.value
	m.cacheRead.valid = m.cacheRead.valid || other.cacheRead.valid
	m.cacheWrite.value += other.cacheWrite.value
	m.cacheWrite.valid = m.cacheWrite.valid || other.cacheWrite.valid
	m.reasoning.value += other.reasoning.value
	m.reasoning.valid = m.reasoning.valid || other.reasoning.valid
	m.premium.value += other.premium.value
	m.premium.valid = m.premium.valid || other.premium.valid
	m.nanoAIU.value += other.nanoAIU.value
	m.nanoAIU.valid = m.nanoAIU.valid || other.nanoAIU.valid
	m.costUSD.value += other.costUSD.value
	m.costUSD.valid = m.costUSD.valid || other.costUSD.valid
	for name := range other.billingModels {
		if m.billingModels == nil {
			m.billingModels = make(map[string]struct{})
		}
		m.billingModels[name] = struct{}{}
	}
	for name := range other.costBases {
		if m.costBases == nil {
			m.costBases = make(map[string]struct{})
		}
		m.costBases[name] = struct{}{}
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
	for identity, runs := range groups {
		repository, externalID := costReferenceParts(identity)
		aggregate := &CostAggregate{Provider: provider, Repository: repository, ExternalKind: kind, ExternalID: externalID}
		runIDs := make([]string, 0, len(runs))
		for runID := range runs {
			runIDs = append(runIDs, runID)
		}
		sort.Strings(runIDs)
		for _, runID := range runIDs {
			run := runs[runID]
			if aggregate.URL == "" {
				if kind == CostExternalKindPR {
					aggregate.URL = run.prs[identity]
				} else {
					aggregate.URL = run.issues[identity]
				}
			}
			aggregate.TotalRuns++
			aggregate.TotalAttempts += run.attempts
			if run.measures.measured() {
				aggregate.MeasuredRuns++
			}
			aggregate.MeasuredAttempts += run.measuredAttempts
			addMeasuresToAggregate(aggregate, run.measures)
			addModelsToAggregate(aggregate, run)
			aggregate.Runs = append(aggregate.Runs, directCostRunAggregate(run))
		}
		sortCostRuns(aggregate.Runs)
		out[identity] = aggregate
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
	addMeasures(costMeasureDestination{
		input: &dst.InputTokens, output: &dst.OutputTokens,
		cacheRead: &dst.CacheReadTokens, cacheWrite: &dst.CacheWriteTokens,
		reasoning: &dst.ReasoningTokens, premium: &dst.CopilotPremiumRequests,
		nanoAIU: &dst.NanoAIU, costUSD: &dst.CostUSD,
		billingModels: &dst.BillingModels, costBases: &dst.CostBases,
	}, src)
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

func directCostRunAggregate(run *costRun) CostRunAggregate {
	aggregate := CostAggregate{}
	addMeasuresToAggregate(&aggregate, run.measures)
	addModelsToAggregate(&aggregate, run)
	return costRunAggregateFrom(run, aggregate)
}

func issueCostRunAggregate(run *costRun, measures costMeasures, targets []string, weights map[string]int64, issue string) CostRunAggregate {
	aggregate := CostAggregate{}
	addMeasuresToAggregate(&aggregate, measures)
	addModelsToIssueAggregate(&aggregate, run, targets, weights, issue)
	return costRunAggregateFrom(run, aggregate)
}

func costRunAggregateFrom(run *costRun, aggregate CostAggregate) CostRunAggregate {
	return CostRunAggregate{
		RunID: run.id, Gaggle: run.gaggle, Workflow: run.workflow, Status: run.status, StartedAt: run.started,
		UsageAttempts: run.attempts, MeasuredAttempts: run.measuredAttempts,
		InputTokens: aggregate.InputTokens, OutputTokens: aggregate.OutputTokens,
		CacheReadTokens: aggregate.CacheReadTokens, CacheWriteTokens: aggregate.CacheWriteTokens,
		ReasoningTokens: aggregate.ReasoningTokens, CopilotPremiumRequests: aggregate.CopilotPremiumRequests,
		NanoAIU: aggregate.NanoAIU, CostUSD: aggregate.CostUSD,
		BillingModels: aggregate.BillingModels, CostBases: aggregate.CostBases, Models: aggregate.Models,
	}
}

func sortCostRuns(runs []CostRunAggregate) {
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].RunID < runs[j].RunID
		}
		return runs[i].StartedAt.Before(runs[j].StartedAt)
	})
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
	addMeasures(costMeasureDestination{
		input: &dst.InputTokens, output: &dst.OutputTokens,
		cacheRead: &dst.CacheReadTokens, cacheWrite: &dst.CacheWriteTokens,
		reasoning: &dst.ReasoningTokens, premium: &dst.CopilotPremiumRequests,
		nanoAIU: &dst.NanoAIU, costUSD: &dst.CostUSD,
		billingModels: &dst.BillingModels, costBases: &dst.CostBases,
	}, src)
}

func addMeasures(dst costMeasureDestination, src costMeasures) {
	addIntPointer(dst.input, src.input)
	addIntPointer(dst.output, src.output)
	addIntPointer(dst.cacheRead, src.cacheRead)
	addIntPointer(dst.cacheWrite, src.cacheWrite)
	addIntPointer(dst.reasoning, src.reasoning)
	addFloatPointer(dst.premium, src.premium)
	addIntPointer(dst.nanoAIU, src.nanoAIU)
	addFloatPointer(dst.costUSD, src.costUSD)
	*dst.billingModels = mergeNames(*dst.billingModels, src.billingModels)
	*dst.costBases = mergeNames(*dst.costBases, src.costBases)
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
	return intsplit.LargestRemainder(value, targets, func(target string) int64 {
		return targetWeight(target, targets, weights)
	})
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

func sortedSet[V any](values map[string]V) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func setsIntersect(left map[string]string, right map[string]struct{}) bool {
	for value := range left {
		if _, ok := right[value]; ok {
			return true
		}
	}
	return false
}

func costReferenceKey(repository, externalID string) string {
	return repository + "\x00" + externalID
}

func costReferenceParts(identity string) (string, string) {
	repository, externalID, _ := strings.Cut(identity, "\x00")
	return repository, externalID
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
