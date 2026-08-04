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

	// Stage, Outcome, and Population are the three filters that used to be
	// residual predicates evaluated after a journal open (#1782). They are
	// answered from run_stage now.
	Stage      string
	Outcome    Outcome
	Population Population

	// IncludeNoWork includes runs whose disposition is DispositionNoWork
	// (#2188). False (the default) excludes them via a plain WHERE term
	// alongside whatever covering index the other dims choose — not yet a
	// dedicated covering index of its own, since that indexed contract is
	// #1429/#1439's to design; see disposition's doc comment in schema.go.
	IncludeNoWork bool

	// OrderBy selects the recency axis (#1777).
	//
	// Two axes, not one, because they answer different operator questions. The
	// default — when a run BEGAN — is what a history page wants. The other is
	// when a run last DID something, which is what an attention list wants: a
	// run that started days ago and escalated a minute ago is the most urgent
	// thing on the instance and the least recent by started_at.
	//
	// Additive: the zero value is the existing behaviour, so no current caller
	// changes meaning by this field existing.
	OrderBy Order
}

// Order is the recency axis a list is filtered and sorted on.
type Order string

const (
	// OrderStartedAt sorts by when the run began. The zero value, so existing
	// callers keep their behaviour.
	OrderStartedAt Order = ""
	// OrderLastActivity sorts by the run's most recent journal event.
	OrderLastActivity Order = "last-activity"
)

// column returns the ordering column for an axis.
func (o Order) column() string {
	if o == OrderLastActivity {
		return "last_activity_at"
	}
	return "started_at"
}

// Population is one of the four measurement filters.
type Population string

// Outcome is a stage-attempt outcome population.
type Outcome string

// OutcomeFinished, OutcomeTerminal, OutcomeSuccess, OutcomeFailure, and
// OutcomeOther are the supported stage-attempt outcome populations.
const (
	OutcomeFinished Outcome = "finished"
	OutcomeTerminal Outcome = "terminal"
	OutcomeSuccess  Outcome = "success"
	OutcomeFailure  Outcome = "failure"
	OutcomeOther    Outcome = "other"
)

// stagePredicate returns the literal predicate matched by an outcome's partial
// index. Values are closed constants, never user-provided SQL.
func (o Outcome) stagePredicate(prefix string) (string, bool) {
	switch o {
	case OutcomeFinished:
		return prefix + "run_terminal = 1 AND (" + prefix + "had_success = 1 OR " +
			prefix + "had_failure = 1 OR " + prefix + "had_other = 1)", true
	case OutcomeTerminal:
		return prefix + "run_terminal = 1 AND (" + prefix + "had_success = 1 OR " +
			prefix + "had_failure = 1)", true
	case OutcomeSuccess:
		return prefix + "run_terminal = 1 AND " + prefix + "had_success = 1", true
	case OutcomeFailure:
		return prefix + "run_terminal = 1 AND " + prefix + "had_failure = 1", true
	case OutcomeOther:
		return prefix + "run_terminal = 1 AND " + prefix + "had_other = 1", true
	default:
		return "", false
	}
}

func (o Outcome) stageIndex(gaggle bool) (string, bool) {
	suffix := string(o)
	switch o {
	case OutcomeFinished, OutcomeTerminal, OutcomeSuccess, OutcomeFailure, OutcomeOther:
	default:
		return "", false
	}
	if gaggle {
		return "idx_run_stage_gaggle_outcome_" + suffix, true
	}
	return "idx_run_stage_outcome_" + suffix, true
}

// The populations, matching the URL values the portal's router parses.
const (
	PopulationTokenMeasured   Population = "token-measured"
	PopulationPremiumMeasured Population = "premium-measured"
	PopulationCostMeasured    Population = "cost-measured"
	PopulationRetryWaste      Population = "retry-waste"
)

// stageColumn returns the run_stage flag column for a population.
func (p Population) stageColumn() (string, bool) {
	switch p {
	case PopulationTokenMeasured:
		return "token_measured", true
	case PopulationPremiumMeasured:
		return "premium_measured", true
	case PopulationCostMeasured:
		return "cost_measured", true
	case PopulationRetryWaste:
		return "retry_waste", true
	default:
		return "", false
	}
}

// runColumn returns the run-level rollup column for a population.
func (p Population) runColumn() (string, bool) {
	col, ok := p.stageColumn()
	if !ok {
		return "", false
	}
	return "any_" + col, true
}

// ListCursor is a keyset position: the ordering key of the last row returned.
//
// Key is the value of whichever column the query ordered by — started_at by
// default, last_activity_at under OrderLastActivity (#1777). It is deliberately
// NOT named for either: a cursor named StartedAt that sometimes held a
// last-activity timestamp is how a paginating client silently skips or repeats
// rows when the axis changes underneath it.
type ListCursor struct {
	Key   time.Time
	RunID string
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
	if o.Stage != "" {
		dims = append(dims, DimStage)
	}
	if o.Outcome != "" {
		dims = append(dims, DimOutcome)
	}
	if o.Population != "" {
		dims = append(dims, DimPopulation)
	}
	if o.OrderBy == OrderLastActivity {
		dims = append(dims, DimActivity)
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
	if options.Outcome != "" {
		if _, ok := options.Outcome.stagePredicate(""); !ok {
			return ListPage{}, fmt.Errorf("readmodel: unknown stage outcome %q", options.Outcome)
		}
	}
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
	db, release, err := s.readHandle()
	if err != nil {
		return ListPage{}, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, query, args...)
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
		key := last.StartedAt
		if options.OrderBy == OrderLastActivity {
			key = last.LastActivity
		}
		page.Next = ListCursor{Key: key, RunID: last.RunID}
	}
	return page, nil
}

// listQuery builds the statement.
//
// Extracted so the plan can be asserted against the statement production runs.
// A plan test that rebuilt the SQL independently would test its own copy, and
// the covering indexes are worth nothing unless the planner picks them for the
// real query.
// listQuery builds the page query for a combination already known to be
// supported.
//
// # Two drivers, chosen by whether a stage is named
//
// Without a stage, the query drives from `run` and every predicate is on a
// leading column of a run index. With a stage, it drives from `run_stage` and
// joins `run` by primary key for the row body — because the stage predicate,
// the gaggle scope, and the ordering must all be served by ONE index, and only
// run_stage has all three (§5.7). Joining the other way round would make the
// stage a residual predicate on an unbounded candidate set, which is the bug
// #1782 reports.
//
// The join costs one primary-key lookup per returned row: limit+1, never more.
// That is the difference from the old candidate loop, which did one journal open
// per row EXAMINED, with nothing bounding how many that was.
func listQuery(options ListOptions, limit int) (string, []any) {
	if options.Stage != "" {
		return stageScopedListQuery(options, limit)
	}
	return runScopedListQuery(options, limit)
}

// runScopedListQuery answers a page with no stage filter, driving from run.
func runScopedListQuery(options ListOptions, limit int) (string, []any) {
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
	if column, ok := options.Population.runColumn(); ok {
		// Interpolated, not bound. The matching index is PARTIAL on this exact
		// literal, and SQLite will not use a partial index whose predicate is
		// expressed with a bound parameter. The value is not user input: it comes
		// from runColumn's closed switch over four constants.
		where = append(where, column+" = 1")
	}
	if !options.IncludeNoWork {
		where = append(where, "disposition != ?")
		args = append(args, DispositionNoWork)
	}
	// The filter, the cursor, and the ORDER BY all use the SAME column (#1777).
	//
	// That is not a tidiness preference. Keyset pagination works by asking for
	// "rows past this position in the sort order"; if since/until bounded
	// started_at while the sort ran on last_activity_at, the predicate would
	// exclude rows the cursor had not yet reached and admit rows it had already
	// passed — so a client paging through would silently skip and repeat runs.
	order := options.OrderBy.column()
	if !options.Since.IsZero() {
		where = append(where, order+" >= ?")
		args = append(args, formatTime(options.Since))
	}
	if !options.Until.IsZero() {
		where = append(where, order+" <= ?")
		args = append(args, formatTime(options.Until))
	}
	if !options.Cursor.Zero() {
		// The keyset predicate mirrors the ordering exactly — descending on the
		// ordering column, ascending on run_id as the tiebreak — so the index can
		// seek straight to the position instead of scanning to it.
		cursorAt := formatTime(options.Cursor.Key)
		where = append(where,
			"("+order+" < ? OR ("+order+" = ? AND run_id > ?))")
		args = append(args, cursorAt, cursorAt, options.Cursor.RunID)
	}

	query := `SELECT ` + runColumns + `
	FROM run r`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY " + order + " DESC, run_id ASC LIMIT ?"
	// limit+1 so the lookahead row answers "is there more" without a COUNT.
	args = append(args, limit+1)
	return query, args
}

// stageScopedListQuery answers a page filtered by stage, driving from run_stage.
//
// Every predicate and the whole ORDER BY are on run_stage columns, so one index
// serves the seek AND the order. The joined run row contributes no predicate —
// if it did, that predicate would be residual and the page would stop being
// bounded.
// Stage-scoped queries are started_at-ordered only. run_stage carries
// run_started_at but not a run_last_activity copy, so ordering them by activity
// would sort against the joined run row — a sort rather than a seek, which is
// the unbounded shape §5.7 refuses. The closed set therefore does not pair
// DimStage with the last-activity axis, and Require refuses the combination
// rather than serving it slowly.
func stageScopedListQuery(options ListOptions, limit int) (string, []any) {
	var (
		where []string
		args  []any
	)
	if options.Gaggle != "" {
		where = append(where, "s.gaggle = ?")
		args = append(args, options.Gaggle)
	}
	where = append(where, "s.stage = ?")
	args = append(args, options.Stage)
	if predicate, ok := options.Outcome.stagePredicate("s."); ok {
		where = append(where, predicate)
	}
	if column, ok := options.Population.stageColumn(); ok {
		// Literal for the same partial-index reason as the run-scoped path.
		where = append(where, "s."+column+" = 1")
	}
	if !options.IncludeNoWork {
		where = append(where, "r.disposition != ?")
		args = append(args, DispositionNoWork)
	}
	if !options.Since.IsZero() {
		where = append(where, "s.run_started_at >= ?")
		args = append(args, formatTime(options.Since))
	}
	if !options.Until.IsZero() {
		where = append(where, "s.run_started_at <= ?")
		args = append(args, formatTime(options.Until))
	}
	if !options.Cursor.Zero() {
		cursorAt := formatTime(options.Cursor.Key)
		where = append(where,
			"(s.run_started_at < ? OR (s.run_started_at = ? AND s.run_id > ?))")
		args = append(args, cursorAt, cursorAt, options.Cursor.RunID)
	}

	from := "run_stage s"
	if index, ok := options.Outcome.stageIndex(options.Gaggle != ""); ok {
		from += " INDEXED BY " + index
	}
	query := `SELECT ` + runColumns + `
	FROM ` + from + ` JOIN run r ON r.run_id = s.run_id
	WHERE ` + strings.Join(where, " AND ") +
		" ORDER BY s.run_started_at DESC, s.run_id ASC LIMIT ?"
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
