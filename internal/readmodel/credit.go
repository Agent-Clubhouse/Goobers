package readmodel

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const defaultCreditLimit = 20

// CreditOptions scopes the cross-run node attribution rollup.
type CreditOptions struct {
	Gaggle   string
	Workflow string
	Since    time.Time
	Until    time.Time
	Limit    int
}

// NodeCredit is one stage identity's accumulated contribution to adverse
// outcomes. Identity includes gaggle and workflow because stage names are only
// unique within a workflow.
type NodeCredit struct {
	Gaggle             string
	Workflow           string
	Stage              string
	RoutedRuns         int
	FailureRuns        int
	EscalationRuns     int
	RetryWasteAttempts int
}

// CreditAssignment returns the highest-contributing graph nodes. Failed and
// escalated runs contribute one point to every node they routed through;
// attempts after the first contribute retry-waste to that node.
func (s *Store) CreditAssignment(ctx context.Context, options CreditOptions) ([]NodeCredit, error) {
	limit := options.Limit
	if limit <= 0 {
		limit = defaultCreditLimit
	}

	predicates := []string{"r.terminal = 1"}
	var args []any
	if options.Gaggle != "" {
		predicates = append(predicates, "r.gaggle = ?")
		args = append(args, options.Gaggle)
	}
	if options.Workflow != "" {
		predicates = append(predicates, "r.workflow = ?")
		args = append(args, options.Workflow)
	}
	if !options.Since.IsZero() {
		predicates = append(predicates, "r.started_at >= ?")
		args = append(args, formatTime(options.Since))
	}
	if !options.Until.IsZero() {
		predicates = append(predicates, "r.started_at <= ?")
		args = append(args, formatTime(options.Until))
	}
	args = append(args, limit)

	query := `
SELECT r.gaggle, r.workflow, rs.stage,
       COUNT(*) AS routed_runs,
       SUM(CASE WHEN r.phase = 'failed' THEN 1 ELSE 0 END) AS failure_runs,
       SUM(CASE WHEN r.phase = 'escalated' THEN 1 ELSE 0 END) AS escalation_runs,
       SUM(CASE WHEN rs.attempts > 1 THEN rs.attempts - 1 ELSE 0 END) AS retry_waste
FROM run_stage rs
JOIN run r ON r.run_id = rs.run_id
WHERE ` + strings.Join(predicates, " AND ") + `
GROUP BY r.gaggle, r.workflow, rs.stage
HAVING failure_runs > 0 OR escalation_runs > 0 OR retry_waste > 0
ORDER BY failure_runs + escalation_runs + retry_waste DESC,
         failure_runs DESC, escalation_runs DESC, retry_waste DESC,
         r.gaggle ASC, r.workflow ASC, rs.stage ASC
LIMIT ?`

	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("readmodel: credit assignment: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []NodeCredit
	for rows.Next() {
		var item NodeCredit
		if err := rows.Scan(
			&item.Gaggle,
			&item.Workflow,
			&item.Stage,
			&item.RoutedRuns,
			&item.FailureRuns,
			&item.EscalationRuns,
			&item.RetryWasteAttempts,
		); err != nil {
			return nil, fmt.Errorf("readmodel: scan credit assignment: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: credit assignment rows: %w", err)
	}
	return result, nil
}
