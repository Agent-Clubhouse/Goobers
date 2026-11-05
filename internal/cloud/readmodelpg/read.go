package readmodelpg

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// ListRuns returns one keyset page from the declared closed query set.
func (s *Store) ListRuns(ctx context.Context, options readmodel.ListOptions) (readmodel.ListPage, error) {
	if _, err := readmodel.Require(options.Dims()); err != nil {
		return readmodel.ListPage{}, err
	}
	if err := validateFilters(options); err != nil {
		return readmodel.ListPage{}, err
	}
	limit := options.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	where := []string{"TRUE"}
	args := make([]any, 0, 12)
	add := func(format string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(format, len(args)))
	}
	if options.Gaggle != "" {
		add("gaggle = $%d", options.Gaggle)
	}
	if options.Workflow != "" {
		add("workflow = $%d", options.Workflow)
	}
	if options.Phase != "" {
		add("phase = $%d", options.Phase)
	}
	if !options.Since.IsZero() {
		add("started_at >= $%d", options.Since)
	}
	if !options.Until.IsZero() {
		add("started_at < $%d", options.Until)
	}
	if !options.IncludeNoWork {
		where = append(where, "disposition <> 'no-work'")
	}
	if options.Stage != "" {
		args = append(args, options.Stage)
		stageWhere := []string{
			"rs.run_id = r.run_id",
			fmt.Sprintf("rs.stage = $%d", len(args)),
		}
		stageWhere = append(stageWhere, stageOutcomePredicates(options.Outcome)...)
		if predicate := stagePopulationPredicate(options.Population); predicate != "" {
			stageWhere = append(stageWhere, predicate)
		}
		where = append(where, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM %s AS rs WHERE %s)",
			s.table("run_stage"), strings.Join(stageWhere, " AND ")))
	} else {
		switch options.Population {
		case readmodel.PopulationTokenMeasured:
			where = append(where, "any_token_measured")
		case readmodel.PopulationPremiumMeasured:
			where = append(where, "any_premium_measured")
		case readmodel.PopulationCostMeasured:
			where = append(where, "any_cost_measured")
		case readmodel.PopulationRetryWaste:
			where = append(where, "any_retry_waste")
		}
	}
	orderColumn := "started_at"
	if options.OrderBy == readmodel.OrderLastActivity {
		orderColumn = "last_activity_at"
	}
	if !options.Cursor.Zero() {
		args = append(args, options.Cursor.Key, options.Cursor.RunID)
		where = append(where, fmt.Sprintf(
			"(%s < $%d OR (%s = $%d AND run_id > $%d))",
			orderColumn, len(args)-1, orderColumn, len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	query := fmt.Sprintf(`
		SELECT projection FROM %s AS r
		WHERE %s
		ORDER BY %s DESC, run_id ASC
		LIMIT $%d`,
		s.table("run"), strings.Join(where, " AND "), orderColumn, len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return readmodel.ListPage{}, fmt.Errorf("readmodelpg: list runs: %w", err)
	}
	defer rows.Close()
	page := readmodel.ListPage{Runs: make([]readmodel.RunRow, 0, limit)}
	for rows.Next() {
		projection, err := scanProjection(rows)
		if err != nil {
			return readmodel.ListPage{}, err
		}
		if len(page.Runs) == limit {
			page.HasMore = true
			continue
		}
		page.Runs = append(page.Runs, projection.Run)
	}
	if err := rows.Err(); err != nil {
		return readmodel.ListPage{}, fmt.Errorf("readmodelpg: list runs rows: %w", err)
	}
	if len(page.Runs) > 0 {
		last := page.Runs[len(page.Runs)-1]
		key := last.StartedAt
		if options.OrderBy == readmodel.OrderLastActivity {
			key = last.LastActivity
			if key.IsZero() {
				key = last.StartedAt
			}
		}
		page.Next = readmodel.ListCursor{Key: key, RunID: last.RunID}
	}
	return page, nil
}

func stageOutcomePredicates(outcome readmodel.Outcome) []string {
	switch outcome {
	case "":
		return nil
	case readmodel.OutcomeFinished:
		return []string{
			"rs.run_terminal",
			"(rs.had_success OR rs.had_failure OR rs.had_other)",
		}
	case readmodel.OutcomeTerminal:
		return []string{"rs.run_terminal", "(rs.had_success OR rs.had_failure)"}
	case readmodel.OutcomeSuccess:
		return []string{"rs.run_terminal", "rs.had_success"}
	case readmodel.OutcomeFailure:
		return []string{"rs.run_terminal", "rs.had_failure"}
	case readmodel.OutcomeOther:
		return []string{"rs.run_terminal", "rs.had_other"}
	default:
		return nil
	}
}

func stagePopulationPredicate(population readmodel.Population) string {
	switch population {
	case readmodel.PopulationTokenMeasured:
		return "rs.token_measured"
	case readmodel.PopulationPremiumMeasured:
		return "rs.premium_measured"
	case readmodel.PopulationCostMeasured:
		return "rs.cost_measured"
	case readmodel.PopulationRetryWaste:
		return "rs.retry_waste"
	default:
		return ""
	}
}

func validateFilters(options readmodel.ListOptions) error {
	switch options.Outcome {
	case "", readmodel.OutcomeFinished, readmodel.OutcomeTerminal,
		readmodel.OutcomeSuccess, readmodel.OutcomeFailure, readmodel.OutcomeOther:
	default:
		return fmt.Errorf("readmodelpg: unknown stage outcome %q", options.Outcome)
	}
	switch options.Population {
	case "", readmodel.PopulationTokenMeasured, readmodel.PopulationPremiumMeasured,
		readmodel.PopulationCostMeasured, readmodel.PopulationRetryWaste:
	default:
		return fmt.Errorf("readmodelpg: unknown population %q", options.Population)
	}
	return nil
}

// LatestPerWorkflow returns the latest qualifying run and active count per
// workflow identity.
func (s *Store) LatestPerWorkflow(
	ctx context.Context,
	options readmodel.AggregateOptions,
) ([]readmodel.WorkflowLatest, error) {
	where := []string{"TRUE"}
	args := make([]any, 0, 2)
	if options.Gaggle != "" {
		args = append(args, options.Gaggle)
		where = append(where, fmt.Sprintf("gaggle = $%d", len(args)))
	}
	if options.Workflow != "" {
		args = append(args, options.Workflow)
		where = append(where, fmt.Sprintf("workflow = $%d", len(args)))
	}
	query := fmt.Sprintf(`
		SELECT projection FROM %s WHERE %s
		ORDER BY started_at DESC, run_id ASC`,
		s.table("run"), strings.Join(where, " AND "))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("readmodelpg: latest per workflow: %w", err)
	}
	defer rows.Close()
	type aggregate struct {
		latest *readmodel.RunRow
		active int
	}
	byWorkflow := make(map[string]*aggregate)
	order := make([]string, 0)
	for rows.Next() {
		projection, err := scanProjection(rows)
		if err != nil {
			return nil, err
		}
		run := projection.Run
		key := run.Gaggle + "\x00" + run.Workflow
		item := byWorkflow[key]
		if item == nil {
			item = &aggregate{}
			byWorkflow[key] = item
			order = append(order, key)
		}
		if !run.Terminal {
			item.active++
		}
		if item.latest == nil && (!options.TerminalOnly || run.Terminal) {
			copy := run
			item.latest = &copy
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodelpg: latest per workflow rows: %w", err)
	}
	out := make([]readmodel.WorkflowLatest, 0, len(order))
	for _, key := range order {
		item := byWorkflow[key]
		if item.latest != nil {
			out = append(out, readmodel.WorkflowLatest{Run: *item.latest, ActiveRuns: item.active})
		}
	}
	return out, nil
}

// CreditAssignment returns PostgreSQL-backed node attribution.
func (s *Store) CreditAssignment(
	ctx context.Context,
	options readmodel.CreditOptions,
) ([]readmodel.NodeCredit, error) {
	projections, _, err := s.loadTerminalProjections(
		ctx, options.Gaggle, options.Workflow, options.Since, options.Until, 0)
	if err != nil {
		return nil, err
	}
	byNode := make(map[readmodel.NodeCreditKey]*readmodel.NodeCredit)
	for _, projection := range projections {
		run := projection.Run
		failed := run.OutcomeTarget == "@abort" ||
			isOneOfFold(run.OutcomeVerdict, "fail", "failure", "reject", "rejected")
		escalated := string(run.Phase) == "escalated" || run.OutcomeTarget == "@escalate"
		for _, node := range projection.Nodes {
			key := readmodel.NodeCreditKey{
				Gaggle: run.Gaggle, Workflow: run.Workflow,
				Kind: node.Kind, Stage: node.Name, Identity: node.Identity,
			}
			item := byNode[key]
			if item == nil {
				item = &readmodel.NodeCredit{
					Gaggle: key.Gaggle, Workflow: key.Workflow,
					Kind: key.Kind, Stage: key.Stage, Identity: key.Identity,
				}
				byNode[key] = item
			}
			item.RoutedRuns++
			if failed {
				item.FailureRuns++
			}
			if escalated {
				item.EscalationRuns++
			}
			item.RetryWasteAttempts += node.RetryWasteAttempts
		}
	}
	out := make([]readmodel.NodeCredit, 0, len(byNode))
	for _, item := range byNode {
		if item.FailureRuns+item.EscalationRuns+item.RetryWasteAttempts > 0 {
			out = append(out, *item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		left := out[i].FailureRuns + out[i].EscalationRuns + out[i].RetryWasteAttempts
		right := out[j].FailureRuns + out[j].EscalationRuns + out[j].RetryWasteAttempts
		if left != right {
			return left > right
		}
		if out[i].FailureRuns != out[j].FailureRuns {
			return out[i].FailureRuns > out[j].FailureRuns
		}
		if out[i].EscalationRuns != out[j].EscalationRuns {
			return out[i].EscalationRuns > out[j].EscalationRuns
		}
		return creditKey(out[i]) < creditKey(out[j])
	})
	limit := options.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CausalCredit returns causal estimates backed by projected experiment facts.
func (s *Store) CausalCredit(
	ctx context.Context,
	options readmodel.CausalOptions,
) ([]readmodel.CausalNodeCredit, error) {
	maxRuns := options.MaxRuns
	if maxRuns <= 0 {
		maxRuns = 10000
	}
	projections, truncated, err := s.loadTerminalProjections(
		ctx, options.Gaggle, options.Workflow, options.Since, options.Until, maxRuns)
	if err != nil {
		return nil, err
	}
	type nodeVersion struct {
		signatures map[string]struct{}
		randomized bool
	}
	nodes := make(map[string]*nodeVersion)
	for _, projection := range projections {
		for _, node := range projection.Nodes {
			key := node.Kind + ":" + node.Name
			item := nodes[key]
			if item == nil {
				item = &nodeVersion{signatures: make(map[string]struct{})}
				nodes[key] = item
			}
			item.randomized = item.randomized || node.Randomized
			if signature := interventionSignature(projection.Run, node.Identity); signature != "" {
				item.signatures[signature] = struct{}{}
			}
		}
	}
	var observations []readmodel.CausalObservation
	for key, version := range nodes {
		if version.randomized {
			for _, projection := range projections {
				node, ok := findNode(projection.Nodes, key)
				if !ok || !node.Randomized {
					continue
				}
				observations = append(observations, readmodel.CausalObservation{
					Node: key, Outcome: adverseOutcome(projection.Run),
					Treated: strings.EqualFold(node.Arm, "treatment"),
					Post:    true, Randomized: true, Arm: node.Arm,
				})
			}
			continue
		}
		if len(version.signatures) < 2 {
			continue
		}
		var before, after string
		var cutoff int
		for index, projection := range projections {
			node, ok := findNode(projection.Nodes, key)
			if !ok {
				continue
			}
			signature := interventionSignature(projection.Run, node.Identity)
			if before == "" {
				before = signature
				continue
			}
			if signature != "" && signature != before {
				after = signature
				cutoff = index
				break
			}
		}
		if before == "" || after == "" {
			continue
		}
		for index, projection := range projections {
			node, routed := findNode(projection.Nodes, key)
			post := index >= cutoff
			if routed {
				signature := interventionSignature(projection.Run, node.Identity)
				if (!post && signature != before) || (post && signature != after) {
					continue
				}
			}
			observations = append(observations, readmodel.CausalObservation{
				Node: key, Outcome: adverseOutcome(projection.Run),
				Treated: routed, Post: post,
			})
		}
	}
	result, err := readmodel.EstimateCausalCredit(observations, options)
	if err != nil {
		return nil, err
	}
	if truncated {
		for i := range result {
			result[i].WindowTruncated = true
			result[i].PromotionEligible = false
			result[i].Caveat = strings.TrimSpace(result[i].Caveat + "; source window truncated")
			result[i].Caveat = strings.TrimPrefix(result[i].Caveat, "; ")
		}
	}
	return result, nil
}

func (s *Store) loadTerminalProjections(
	ctx context.Context,
	gaggle string,
	workflow string,
	since, until time.Time,
	limit int,
) ([]readmodel.Projection, bool, error) {
	where := []string{"terminal"}
	args := make([]any, 0, 5)
	add := func(format string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(format, len(args)))
	}
	if gaggle != "" {
		add("gaggle = $%d", gaggle)
	}
	if workflow != "" {
		add("workflow = $%d", workflow)
	}
	if !since.IsZero() {
		add("started_at >= $%d", since)
	}
	if !until.IsZero() {
		add("started_at <= $%d", until)
	}
	query := fmt.Sprintf(
		`SELECT projection FROM %s WHERE %s ORDER BY started_at DESC, run_id ASC`,
		s.table("run"), strings.Join(where, " AND "))
	if limit > 0 {
		args = append(args, limit+1)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("readmodelpg: load terminal projections: %w", err)
	}
	defer rows.Close()
	var projections []readmodel.Projection
	for rows.Next() {
		projection, err := scanProjection(rows)
		if err != nil {
			return nil, false, err
		}
		projections = append(projections, projection)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("readmodelpg: load terminal projection rows: %w", err)
	}
	truncated := limit > 0 && len(projections) > limit
	if truncated {
		projections = projections[:limit]
	}
	sort.Slice(projections, func(i, j int) bool {
		if projections[i].Run.StartedAt.Equal(projections[j].Run.StartedAt) {
			return projections[i].Run.RunID < projections[j].Run.RunID
		}
		return projections[i].Run.StartedAt.Before(projections[j].Run.StartedAt)
	})
	return projections, truncated, nil
}

func creditKey(item readmodel.NodeCredit) string {
	return strings.Join([]string{
		item.Gaggle, item.Workflow, item.Kind, item.Stage, item.Identity,
	}, "\x00")
}

func isOneOfFold(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}

func interventionSignature(run readmodel.RunRow, identity string) string {
	if identity == "" && run.WorkflowVersion == 0 && run.WorkflowDigest == "" {
		return ""
	}
	return strings.Join([]string{
		identity, strconv.Itoa(run.WorkflowVersion), run.WorkflowDigest,
	}, "\x00")
}

func findNode(nodes []readmodel.NodeRow, key string) (readmodel.NodeRow, bool) {
	for _, node := range nodes {
		if node.Kind+":"+node.Name == key {
			return node, true
		}
	}
	return readmodel.NodeRow{}, false
}

func adverseOutcome(run readmodel.RunRow) float64 {
	if run.OutcomeTarget == "@abort" ||
		isOneOfFold(run.OutcomeVerdict, "fail", "failure", "reject", "rejected") {
		return 1
	}
	return 0
}

// CountByPhase returns the run histogram in one aggregate query.
func (s *Store) CountByPhase(ctx context.Context) (map[journal.RunPhase]int, error) {
	query := fmt.Sprintf(`SELECT phase, count(*) FROM %s GROUP BY phase`, s.table("run"))
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("readmodelpg: count by phase: %w", err)
	}
	defer rows.Close()
	out := make(map[journal.RunPhase]int)
	for rows.Next() {
		var phase journal.RunPhase
		var count int
		if err := rows.Scan(&phase, &count); err != nil {
			return nil, fmt.Errorf("readmodelpg: scan phase count: %w", err)
		}
		out[phase] = count
	}
	return out, rows.Err()
}

// ActiveRunCounts returns non-terminal counts by workflow.
func (s *Store) ActiveRunCounts(ctx context.Context) ([]readmodel.WorkflowCount, error) {
	query := fmt.Sprintf(`
		SELECT gaggle, workflow, count(*) FROM %s
		WHERE NOT terminal GROUP BY gaggle, workflow
		ORDER BY gaggle, workflow`, s.table("run"))
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("readmodelpg: active run counts: %w", err)
	}
	defer rows.Close()
	var out []readmodel.WorkflowCount
	for rows.Next() {
		var count readmodel.WorkflowCount
		if err := rows.Scan(&count.Gaggle, &count.Workflow, &count.Count); err != nil {
			return nil, fmt.Errorf("readmodelpg: scan active count: %w", err)
		}
		out = append(out, count)
	}
	return out, rows.Err()
}

// Changes returns committed transitions after the caller's position.
func (s *Store) Changes(ctx context.Context, afterSeq uint64, limit int) ([]readmodel.Change, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := fmt.Sprintf(`
		SELECT seq, changed_at, kind, run_id, gaggle, workflow
		FROM %s WHERE seq > $1 ORDER BY seq LIMIT $2`, s.table("change"))
	rows, err := s.pool.Query(ctx, query, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("readmodelpg: changes: %w", err)
	}
	defer rows.Close()
	out := make([]readmodel.Change, 0, limit)
	for rows.Next() {
		var change readmodel.Change
		if err := rows.Scan(
			&change.Seq, &change.At, &change.Kind, &change.RunID,
			&change.Gaggle, &change.Workflow,
		); err != nil {
			return nil, fmt.Errorf("readmodelpg: scan change: %w", err)
		}
		out = append(out, change)
	}
	return out, rows.Err()
}

// LatestChangeSeq reports the newest allocated sequence.
func (s *Store) LatestChangeSeq(ctx context.Context) (uint64, error) {
	query := fmt.Sprintf(`SELECT next_change_seq FROM %s WHERE singleton`, s.table("meta"))
	var seq uint64
	if err := s.pool.QueryRow(ctx, query).Scan(&seq); err != nil {
		return 0, fmt.Errorf("readmodelpg: latest change sequence: %w", err)
	}
	return seq, nil
}

func scanProjection(row interface{ Scan(...any) error }) (readmodel.Projection, error) {
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		return readmodel.Projection{}, fmt.Errorf("readmodelpg: scan projection: %w", err)
	}
	var projection readmodel.Projection
	if err := json.Unmarshal(raw, &projection); err != nil {
		return readmodel.Projection{}, fmt.Errorf("readmodelpg: decode projection: %w", err)
	}
	return projection, nil
}
