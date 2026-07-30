package readmodel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// The bounded list query (#1918/#1920, design §5.7, §7.3).
//
// This is what Wave 2 exists for. Measured at 1x AFTER Wave 1, a 50-row page
// still opened 51 journals — one per returned row plus the lookahead — because
// the row in telemetry.db was not complete enough to answer the list contract
// and each run had to be hydrated from its journal. That is the diagnosis's
// "lists open and parse a journal per returned row", and it is the last
// uncorrected item from it.
//
// Every column the contract needs is in the run row (§5.3), so this query opens
// nothing.
//
// # Keyset, never offset
//
// The cursor encodes the ordering key (started_at, run_id) rather than a row
// count, so page N costs what page 1 costs. An offset plan degrades with depth,
// and a first-page-only measurement never sees it — which is why the Wave 0
// harness times a deep page separately.
//
// # Every query carries its scope predicate
//
// §5.5: filtering AFTER limit silently omits rows (the diagnosis's §5.6
// failure), and filtering BEFORE limit without an index reintroduces the scan.
// So the gaggle predicate is inside the indexed query, which is also why gaggle
// leads the scoped ordering indexes.

// ListOptions is a bounded list request.
type ListOptions struct {
	Gaggle   string
	Workflow string
	Phase    journal.RunPhase
	Since    time.Time
	Until    time.Time
	Limit    int
	Cursor   ListCursor
}

// ListCursor is a keyset position: the ordering key of the last row returned.
type ListCursor struct {
	StartedAt time.Time
	RunID     string
}

// Zero reports whether the cursor is unset — a first page.
func (c ListCursor) Zero() bool { return c.RunID == "" }

// ListPage is one page plus the cursor that continues it.
type ListPage struct {
	Runs []RunRow
	Next ListCursor
	// HasMore is derived from the limit+1 lookahead row, so "is there another
	// page" never costs a second query or a COUNT.
	HasMore bool
}

// Dims reports which filter dimensions the options constrain, for the closed-set
// check.
func (o ListOptions) Dims() []Dim {
	var dims []Dim
	if o.Gaggle != "" {
		dims = append(dims, DimGaggle)
	}
	if o.Workflow != "" {
		dims = append(dims, DimWorkflow)
	}
	if o.Phase != "" {
		dims = append(dims, DimPhase)
	}
	if !o.Since.IsZero() {
		dims = append(dims, DimSince)
	}
	if !o.Until.IsZero() {
		dims = append(dims, DimUntil)
	}
	return dims
}

// defaultListLimit and maxListLimit are the page sizes the interface renders.
//
// §15.9 classifies these as a REQUIREMENT rather than an accident: "50 / 200
// page limits — Requirement. What the interface renders."
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// ListRuns returns one bounded page.
//
// It refuses any filter combination outside the closed set (§5.7) BEFORE
// touching the database. Refusing early matters: the point of the closed set is
// that an unsupported combination is never executed, and validating after the
// query would mean the expensive thing already happened.
func (s *Store) ListRuns(ctx context.Context, options ListOptions) (ListPage, error) {
	combination, err := Require(options.Dims())
	if err != nil {
		return ListPage{}, err
	}
	_ = combination // the index it names is asserted by the plan tests

	limit := options.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	query, args := listQuery(options, limit)
	rows, err := s.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return ListPage{}, fmt.Errorf("readmodel: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := ListPage{Runs: make([]RunRow, 0, limit)}
	for rows.Next() {
		row, err := scanRunRow(rows)
		if err != nil {
			return ListPage{}, err
		}
		// The lookahead row is read but never returned: it exists only to answer
		// "is there more" without a second query.
		if len(page.Runs) == limit {
			page.HasMore = true
			break
		}
		page.Runs = append(page.Runs, row)
	}
	if err := rows.Err(); err != nil {
		return ListPage{}, fmt.Errorf("readmodel: list rows: %w", err)
	}
	if len(page.Runs) > 0 {
		last := page.Runs[len(page.Runs)-1]
		page.Next = ListCursor{StartedAt: last.StartedAt, RunID: last.RunID}
	}
	return page, nil
}

// listQuery builds the statement.
//
// Extracted so the plan can be asserted against the statement production runs.
// A plan test that rebuilt the SQL independently would test its own copy, and
// the covering indexes are worth nothing unless the planner picks them for the
// real query.
func listQuery(options ListOptions, limit int) (string, []any) {
	var (
		where []string
		args  []any
	)
	// Equality predicates first, matching the leading columns of the ordering
	// indexes. Order here is cosmetic to SQLite but keeps the statement readable
	// alongside the index definitions.
	if options.Gaggle != "" {
		where = append(where, "gaggle = ?")
		args = append(args, options.Gaggle)
	}
	if options.Workflow != "" {
		where = append(where, "workflow = ?")
		args = append(args, options.Workflow)
	}
	if options.Phase != "" {
		where = append(where, "phase = ?")
		args = append(args, string(options.Phase))
	}
	if !options.Since.IsZero() {
		where = append(where, "started_at >= ?")
		args = append(args, formatTime(options.Since))
	}
	if !options.Until.IsZero() {
		where = append(where, "started_at <= ?")
		args = append(args, formatTime(options.Until))
	}
	if !options.Cursor.Zero() {
		// The keyset predicate mirrors the ordering exactly — descending on
		// started_at, ascending on run_id as the tiebreak — so the index can seek
		// straight to the position instead of scanning to it.
		cursorAt := formatTime(options.Cursor.StartedAt)
		where = append(where, "(started_at < ? OR (started_at = ? AND run_id > ?))")
		args = append(args, cursorAt, cursorAt, options.Cursor.RunID)
	}

	query := `SELECT ` + runColumns + `
	FROM run r`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY started_at DESC, run_id ASC LIMIT ?"
	// limit+1 so the lookahead row answers "is there more" without a COUNT.
	args = append(args, limit+1)
	return query, args
}

// Duration returns a run's elapsed time as of observedAt.
//
// Computed rather than stored, because a running run's duration is now-relative:
// a stored value would freeze at projection time and a quiet in-flight run would
// stop ageing (§5.3). This is the query-time half of that decision.
func (r RunRow) Duration(observedAt time.Time) time.Duration {
	end := observedAt
	if r.FinishedAt != nil {
		end = *r.FinishedAt
	}
	if end.Before(r.StartedAt) {
		return 0
	}
	return end.Sub(r.StartedAt)
}
