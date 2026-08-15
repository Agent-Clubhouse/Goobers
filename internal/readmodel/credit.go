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

// NodeCredit is one graph node's accumulated contribution to adverse outcomes.
// Gaggle and workflow are part of the identity because node names are only
// unique within a workflow. Identity is populated only when the journal carries
// a node-specific prompt or tool identity.
type NodeCredit struct {
	Gaggle             string
	Workflow           string
	Kind               string
	Stage              string
	Identity           string
	RoutedRuns         int
	FailureRuns        int
	EscalationRuns     int
	RetryWasteAttempts int
}

// CreditAssignment returns the highest-contributing graph nodes.
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
SELECT r.gaggle, r.workflow, rn.kind, rn.name, rn.identity,
       COUNT(*) AS routed_runs,
       SUM(CASE WHEN r.outcome_target = '@abort'
                     OR lower(r.outcome_verdict) IN ('fail', 'failure', 'reject', 'rejected')
                THEN 1 ELSE 0 END) AS failure_runs,
       SUM(CASE WHEN r.phase = 'escalated' OR r.outcome_target = '@escalate'
                THEN 1 ELSE 0 END) AS escalation_runs,
       SUM(rn.retry_waste_attempts) AS retry_waste
FROM run_node rn
JOIN run r ON r.run_id = rn.run_id
WHERE ` + strings.Join(predicates, " AND ") + `
GROUP BY r.gaggle, r.workflow, rn.kind, rn.name, rn.identity
HAVING failure_runs > 0 OR escalation_runs > 0 OR retry_waste > 0
ORDER BY failure_runs + escalation_runs + retry_waste DESC,
         failure_runs DESC, escalation_runs DESC, retry_waste DESC,
         r.gaggle ASC, r.workflow ASC, rn.kind ASC, rn.name ASC, rn.identity ASC
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
			&item.Kind,
			&item.Stage,
			&item.Identity,
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
