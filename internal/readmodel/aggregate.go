package readmodel

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The latest-outcome aggregate (#1891, design §5.2/§14.4).
//
// The Workflows page needs, for each workflow, its most recent run. Three
// separate unbounded paths answer that today:
//
//   - rollup/query.go's LatestWorkflowRunRefs is a window function
//     (ROW_NUMBER() OVER (PARTITION BY gaggle, workflow ...)) over ALL of runs —
//     §5.2's "unindexed window function over all history".
//   - readservice's backwards terminal walk (§12 lists it for deletion) opens
//     journals until it finds a terminal run per workflow.
//   - and, until #1741, the whole thing also reached the active-run scan.
//
// §14.4's property is that "a 2,000-workflow page issues one aggregate request
// and zero per-workflow requests", and that the cost does not grow with workflow
// count. One request was already true after #1894; what was missing is that the
// single request is BOUNDED.
//
// # Why this is not a window function
//
// A correlated MAX over the ordering index seeks directly to each workflow's
// newest row, which is what the index is shaped for: (gaggle, workflow,
// started_at DESC, run_id). A window function has to rank every row in the
// partition before discarding all but the first — it reads the whole history to
// return one row per workflow.
//
// # Why it is scoped to a gaggle
//
// Same reason gaggle leads the ordering indexes (§5.5): the scope has to be a
// predicate inside the indexed query. An unscoped aggregate over every gaggle is
// available for the unrestricted principal, but a scoped one must not be
// answered by filtering an unscoped result.

// WorkflowLatest is one workflow's most recent run, complete.
//
// Run is the FULL row, not a reference. That is the difference between this and
// what it replaces: rollup's aggregate returned a (run_id, started_at) reference
// and the caller then opened that run's journal to fill in the rest — so a
// 2,000-workflow page issued one aggregate request and 2,000 journal opens,
// which is precisely the shape §14.4 says must not happen. Selecting the whole
// row in the same query is what makes the per-workflow cost zero rather than
// merely smaller.
type WorkflowLatest struct {
	Run RunRow
	// ActiveRuns is how many of this workflow's runs are currently running —
	// the number the concurrency ceiling is compared against, answered here as
	// an indexed aggregate rather than by walking directories (§5.4).
	//
	// It is NOT filtered by terminality even when the outcome is: "what did this
	// workflow last do" and "what is it doing" are different questions about the
	// same workflow, and the page asks both at once.
	ActiveRuns int
}

// AggregateOptions scopes the latest-outcome aggregate.
type AggregateOptions struct {
	// Gaggle and Workflow scope the result. Empty means unrestricted, which is
	// the fast path for a principal provably scoped to everything (§5.5).
	Gaggle   string
	Workflow string
	// TerminalOnly asks for each workflow's most recent run that actually
	// FINISHED, rather than its most recent run.
	//
	// This is the distinction the Workflows page depends on and the one easiest
	// to lose: "what did this workflow last do" is not "what is it doing". A
	// workflow whose newest run is still in flight has to keep showing its last
	// outcome, or the page goes blank exactly when someone is watching it.
	TerminalOnly bool
}

// LatestPerWorkflow returns each workflow's most recent run, plus its active-run
// count, in one query.
func (s *Store) LatestPerWorkflow(ctx context.Context, options AggregateOptions) ([]WorkflowLatest, error) {
	query, args := latestPerWorkflowQuery(options)
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("readmodel: latest per workflow: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkflowLatest
	for rows.Next() {
		// The row is scanned by the SAME helper the list uses, against the same
		// column list. Sharing it is what keeps the Workflows page and the Runs
		// page from drifting into two different ideas of what a run looks like.
		row, active, err := scanAggregateRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, WorkflowLatest{Run: row, ActiveRuns: active})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: latest per workflow rows: %w", err)
	}
	return out, nil
}

// latestPerWorkflowQuery builds the aggregate.
//
// Extracted so the plan is asserted against the statement production runs — an
// index that exists but is not chosen costs write amplification while the read
// stays a scan, and nothing fails.
//
// The shape is a self-join on the per-workflow maximum rather than a window
// function. The inner GROUP BY seeks each (gaggle, workflow) group's newest
// started_at through the ordering index; the join then fetches that one row.
// Ties on started_at are broken by MIN(run_id), matching the list's ordering
// (started_at DESC, run_id ASC) so the aggregate and the list agree about which
// run is "latest" — disagreeing would make the Workflows page and the Runs page
// tell different stories about the same workflow.
func latestPerWorkflowQuery(options AggregateOptions) (string, []any) {
	// The scope predicates are built once and reused by each CTE, so the pick and
	// the active count can never disagree about what they are counting.
	var (
		scope     []string
		scopeArgs []any
	)
	if options.Gaggle != "" {
		scope = append(scope, "gaggle = ?")
		scopeArgs = append(scopeArgs, options.Gaggle)
	}
	if options.Workflow != "" {
		scope = append(scope, "workflow = ?")
		scopeArgs = append(scopeArgs, options.Workflow)
	}

	// Terminality is applied to the PICK but never to the active count: the two
	// answer different questions about the same workflow, and a workflow with a
	// finished run last week and a running run right now must report both.
	pickWhere := append([]string{}, scope...)
	// pickJoin repeats the terminality test on the row-fetch join. Without it,
	// `newest` finds the newest TERMINAL started_at but the join could match a
	// non-terminal run that started at the same instant and hand back its run_id
	// — a wrong answer that only appears under a timestamp tie, which is exactly
	// the kind of bug that survives every test that does not construct one.
	pickJoin := ""
	if options.TerminalOnly {
		// Literal rather than a bound parameter so SQLite can match it against
		// the partial index's WHERE clause — a bound `terminal = ?` does not
		// qualify, and the index silently goes unused.
		pickWhere = append(pickWhere, "terminal = 1")
		pickJoin = " AND r.terminal = 1"
	}
	activeWhere := append([]string{"phase = 'running'"}, scope...)

	args := append([]any{}, scopeArgs...)
	args = append(args, scopeArgs...)

	// The active-run count is a second scoped aggregate rather than a correlated
	// subquery per row: one indexed GROUP BY over phase='running' costs the same
	// whether there are two active runs or two thousand workflows.
	query := `
WITH newest AS (
	SELECT gaggle, workflow, MAX(started_at) AS started_at
	FROM run` + whereClause(pickWhere) + `
	GROUP BY gaggle, workflow
),
pick AS (
	SELECT r.gaggle, r.workflow, MIN(r.run_id) AS run_id, n.started_at
	FROM newest n
	JOIN run r ON r.gaggle = n.gaggle AND r.workflow = n.workflow AND r.started_at = n.started_at` + pickJoin + `
	GROUP BY r.gaggle, r.workflow, n.started_at
),
active AS (
	SELECT gaggle, workflow, COUNT(*) AS running
	FROM run` + whereClause(activeWhere) + `
	GROUP BY gaggle, workflow
)
SELECT ` + runColumns + `, COALESCE(a.running, 0)
FROM pick p
JOIN run r ON r.run_id = p.run_id
LEFT JOIN active a ON a.gaggle = p.gaggle AND a.workflow = p.workflow
ORDER BY p.gaggle ASC, p.workflow ASC`

	return query, args
}

// scanAggregateRow decodes one aggregate row: a complete run plus its active
// count.
func scanAggregateRow(rows *sql.Rows) (RunRow, int, error) {
	var (
		row    RunRow
		active int
	)
	targets := append(runScanTargets(&row), &active)
	if err := rows.Scan(targets...); err != nil {
		return RunRow{}, 0, fmt.Errorf("readmodel: scan latest per workflow: %w", err)
	}
	if err := row.finishScan(); err != nil {
		return RunRow{}, 0, err
	}
	return row, active, nil
}

// whereClause renders predicates, or nothing when there are none.
func whereClause(predicates []string) string {
	if len(predicates) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(predicates, " AND ")
}
