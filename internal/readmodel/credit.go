package readmodel

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const defaultCreditLimit = 20

// CreditOptions scopes the cross-run node attribution rollup.
//
// This file is the PRODUCTION credit-attribution path: `goobers telemetry
// query --aggregate credit-assignment`, the read API, and the portal all read
// from here. internal/creditgraph is the separate per-run attribution path.
// Where the two overlap (routed stages, cause location, aborted runs)
// they must agree: internal/creditgraph/readmodel_conformance_test.go pins it,
// and docs/design/credit-graph.md ("Conformance and compatibility", #6355)
// documents the overlap. A change to what counts as a failure here must update
// that test in the same commit.
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

// NodeCreditKey is a NodeCredit's identity: the columns CreditAssignment
// groups by. It keys CreditAssignmentRunIDs' result.
type NodeCreditKey struct {
	Gaggle   string
	Workflow string
	Kind     string
	Stage    string
	Identity string
}

// Key returns the node's grouping identity.
func (n NodeCredit) Key() NodeCreditKey {
	return NodeCreditKey{Gaggle: n.Gaggle, Workflow: n.Workflow, Kind: n.Kind, Stage: n.Stage, Identity: n.Identity}
}

// creditRunIDBatchNodes bounds how many nodes one evidence query names, so the
// bound-parameter count stays far below SQLite's limit whatever the caller
// passes. CreditAssignment returns defaultCreditLimit nodes by default, so a
// telemetry request normally takes exactly one query.
const creditRunIDBatchNodes = 100

// CreditAssignmentRunIDs returns bounded journal references for each attributed
// node, in the same window as CreditAssignment: at most limit run IDs per node,
// newest first. Every requested node is answered by one grouped query per
// creditRunIDBatchNodes nodes rather than one query per node (#4572). A node
// with no matching terminal run is absent from the map.
func (s *Store) CreditAssignmentRunIDs(ctx context.Context, options CreditOptions, nodes []NodeCredit, limit int) (map[NodeCreditKey][]string, error) {
	if limit <= 0 {
		limit = defaultCreditLimit
	}
	unique := make([]NodeCredit, 0, len(nodes))
	seen := make(map[NodeCreditKey]bool, len(nodes))
	for _, node := range nodes {
		if !seen[node.Key()] {
			seen[node.Key()] = true
			unique = append(unique, node)
		}
	}
	nodes = unique
	result := make(map[NodeCreditKey][]string, len(nodes))
	for start := 0; start < len(nodes); start += creditRunIDBatchNodes {
		end := min(start+creditRunIDBatchNodes, len(nodes))
		if err := s.creditAssignmentRunIDBatch(ctx, options, nodes[start:end], limit, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) creditAssignmentRunIDBatch(ctx context.Context, options CreditOptions, nodes []NodeCredit, limit int, into map[NodeCreditKey][]string) error {
	values := make([]string, 0, len(nodes))
	args := make([]any, 0, len(nodes)*5+3)
	for _, node := range nodes {
		values = append(values, "(?, ?, ?, ?, ?)")
		args = append(args, node.Gaggle, node.Workflow, node.Kind, node.Stage, node.Identity)
	}
	predicates := []string{"r.terminal = 1"}
	if !options.Since.IsZero() {
		predicates = append(predicates, "r.started_at >= ?")
		args = append(args, formatTime(options.Since))
	}
	if !options.Until.IsZero() {
		predicates = append(predicates, "r.started_at <= ?")
		args = append(args, formatTime(options.Until))
	}
	args = append(args, limit)
	query := `WITH wanted(gaggle, workflow, kind, name, identity) AS (VALUES ` + strings.Join(values, ", ") + `),
ranked AS (
	SELECT w.gaggle, w.workflow, w.kind, w.name, w.identity, r.run_id, r.started_at,
	       ROW_NUMBER() OVER (
	           PARTITION BY w.gaggle, w.workflow, w.kind, w.name, w.identity
	           ORDER BY r.started_at DESC, r.run_id DESC) AS evidence_rank
	FROM wanted w
	JOIN run_node rn ON rn.kind = w.kind AND rn.name = w.name AND rn.identity = w.identity
	JOIN run r ON r.run_id = rn.run_id AND r.gaggle = w.gaggle AND r.workflow = w.workflow
	WHERE ` + strings.Join(predicates, " AND ") + `
)
SELECT gaggle, workflow, kind, name, identity, run_id FROM ranked
WHERE evidence_rank <= ?
ORDER BY gaggle, workflow, kind, name, identity, evidence_rank`
	return s.withReadRows(ctx, query, args,
		"readmodel: credit assignment run ids",
		"readmodel: credit assignment run ids rows",
		func(rows *sql.Rows) error {
			var key NodeCreditKey
			var runID string
			if err := rows.Scan(&key.Gaggle, &key.Workflow, &key.Kind, &key.Stage, &key.Identity, &runID); err != nil {
				return fmt.Errorf("readmodel: scan credit assignment run id: %w", err)
			}
			into[key] = append(into[key], runID)
			return nil
		})
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

	var result []NodeCredit
	err := s.withReadRows(ctx, query, args,
		"readmodel: credit assignment",
		"readmodel: credit assignment rows",
		func(rows *sql.Rows) error {
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
				return fmt.Errorf("readmodel: scan credit assignment: %w", err)
			}
			result = append(result, item)
			return nil
		})
	if err != nil {
		return nil, err
	}
	return result, nil
}
