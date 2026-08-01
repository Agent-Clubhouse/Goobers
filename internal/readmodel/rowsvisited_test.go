package readmodel

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// The rows-visited harness (#1920, design §14.2).
//
// # Why `limit+1` returned rows is not a bound
//
// §5.7's central finding, and the reason the closed set exists at all: the
// number of rows a query RETURNS says nothing about how many it EXAMINED. A
// selective residual predicate lets SQLite walk thousands of recency candidates
// before it accumulates 51 matches — which is the old candidate loop relocated
// into the query planner rather than removed. The page still comes back in 51
// rows, and the request still reads the whole history.
//
// `EXPLAIN QUERY PLAN` cannot close the gap either. It reports access METHODS,
// not cardinalities, and it does not enumerate residual terms — SQLite will
// print `SEARCH run USING INDEX idx_ab (a=?)` while silently evaluating `c=?` on
// every row it touches. A plan assertion can tell you an index was used; it
// cannot tell you how many rows went past.
//
// So this counts them directly. A non-deterministic scalar function is injected
// into the WHERE clause and increments once per row SQLite actually evaluates it
// on.
//
// # Why the function must be non-deterministic
//
// A deterministic function is one SQLite is free to cache, factor out of a loop,
// or evaluate once for a constant argument. Any of those would make the counter
// undercount, and it would undercount SILENTLY — reporting a beautiful bound
// that the shipping query does not have. `RegisterScalarFunction` registers with
// `Deterministic: false`, which is the whole reason to use it over
// `RegisterDeterministicScalarFunction`.
//
// # Why the plan is asserted identical
//
// Adding a term to a WHERE clause can change the plan. If it does, the harness
// measures a different query from the one that ships, and a passing bound means
// nothing. So the instrumented query's plan is compared against the production
// query's, and a divergence fails the test rather than being reported as a
// measurement.

var (
	probeOnce  sync.Once
	probeCount atomic.Int64
	probeErr   error
)

// registerRowProbe installs `probe(x)` once per process.
//
// Registration is global to the driver and affects connections opened AFTER it,
// which is why it happens before any store is opened rather than lazily.
func registerRowProbe(t *testing.T) {
	t.Helper()
	probeOnce.Do(func() {
		probeErr = sqlite.RegisterScalarFunction("probe", 1,
			func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
				probeCount.Add(1)
				// Returns 1 so `AND probe(x) = 1` never filters anything out. The
				// probe must not change the result set, only observe it.
				return int64(1), nil
			})
	})
	if probeErr != nil {
		t.Fatalf("register probe: %v", probeErr)
	}
}

// instrument rewrites a list query to call probe() once per examined row.
//
// The term is appended to the WHERE clause rather than the SELECT list on
// purpose: a SELECT-list expression is evaluated only for rows that survive
// filtering and pagination, which would count returned rows and prove nothing.
// In the WHERE clause it runs on every row the query actually reaches.
func instrument(query string) (string, bool) {
	// The probe binds to the driving table's row id: `r.run_id` for run-scoped
	// queries, `s.run_id` for stage-scoped ones.
	column := "r.run_id"
	if strings.Contains(query, "FROM run_stage s") {
		column = "s.run_id"
	}
	marker := " ORDER BY "
	at := strings.LastIndex(query, marker)
	if at < 0 {
		return "", false
	}
	head, tail := query[:at], query[at:]
	if strings.Contains(head, "WHERE") {
		head += " AND probe(" + column + ") = 1"
	} else {
		head += " WHERE probe(" + column + ") = 1"
	}
	return head + tail, true
}

// TestEveryCombinationVisitsAtMostLimitPlusOneRows is §14.2's bound, measured.
func TestEveryCombinationVisitsAtMostLimitPlusOneRows(t *testing.T) {
	registerRowProbe(t)

	store := openTestStore(t)
	seedProbeCorpus(t, store, 3_000)

	const limit = 50
	for _, combination := range SupportedCombinations() {
		options, ok := probeOptionsFor(combination.Dims, limit)
		if !ok {
			// Not silently skipped: a combination the harness cannot express is a
			// combination with no measured bound.
			t.Errorf("harness cannot express {%s}; its rows-visited bound would go unmeasured",
				Key(combination.Dims))
			continue
		}

		t.Run(Key(combination.Dims), func(t *testing.T) {
			query, args := listQuery(options, limit)
			instrumented, ok := instrument(query)
			if !ok {
				t.Fatalf("could not instrument query: %s", query)
			}

			// The plan must not move. If instrumenting changed it, the number
			// below describes a query that does not ship.
			production := explainWithArgs(t, store, query, args)
			measured := explainWithArgs(t, store, instrumented, args)
			if production != measured {
				t.Fatalf("instrumenting changed the plan, so the count would describe a "+
					"different query:\n production: %s\n instrumented: %s", production, measured)
			}

			probeCount.Store(0)
			rows, err := store.readDB().Query(instrumented, args...)
			if err != nil {
				t.Fatalf("instrumented query: %v", err)
			}
			returned := 0
			for rows.Next() {
				returned++
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				t.Fatalf("scan: %v", err)
			}
			_ = rows.Close()

			visited := probeCount.Load()
			t.Logf("visited %d rows, returned %d (limit %d)", visited, returned, limit)

			if visited > int64(limit+1) {
				t.Errorf("visited %d rows for limit %d; §5.7's bound is limit+1 = %d.\n"+
					"A residual predicate is being evaluated on rows the index should have "+
					"excluded — which is the candidate loop relocated into the planner, not removed.",
					visited, limit, limit+1)
			}
			if visited == 0 {
				t.Error("the probe was never called; the harness is measuring nothing")
			}
		})
	}
}

// TestProbeCountsExaminedRowsNotReturnedRows is the harness's own test.
//
// A rows-visited harness that actually counted RETURNED rows would pass every
// bound trivially and prove nothing — it would report `limit+1` for a query that
// walked the entire table. So the probe is pointed at a query with a deliberate
// residual predicate and must show the walk.
func TestProbeCountsExaminedRowsNotReturnedRows(t *testing.T) {
	registerRowProbe(t)
	store := openTestStore(t)
	seedProbeCorpus(t, store, 2_000)

	// `goober_digest` has no index, so this predicate is residual by
	// construction: SQLite must evaluate it on every row of the recency scan.
	// One row matches.
	query := `SELECT run_id FROM run r
		WHERE goober_digest = 'needle' AND probe(r.run_id) = 1
		ORDER BY started_at DESC, run_id ASC LIMIT 51`

	probeCount.Store(0)
	rows, err := store.readDB().Query(query)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	returned := 0
	for rows.Next() {
		returned++
	}
	_ = rows.Close()

	visited := probeCount.Load()
	t.Logf("residual predicate: visited %d rows, returned %d", visited, returned)

	if returned > 1 {
		t.Fatalf("fixture returned %d rows, want at most 1", returned)
	}
	// The point: few rows out, many rows examined. If the probe reported
	// something near `returned`, it would be counting the wrong thing and every
	// bound it certified would be worthless.
	if visited < 1_000 {
		t.Errorf("probe counted %d rows for a full residual scan of 2,000; it is counting "+
			"returned rows rather than examined ones, which would make every bound vacuous",
			visited)
	}
}

// probeOptionsFor builds a list request for a combination, at a fixed limit.
func probeOptionsFor(dims []Dim, limit int) (ListOptions, bool) {
	options := ListOptions{Limit: limit}
	for _, dim := range dims {
		switch dim {
		case DimGaggle:
			options.Gaggle = "gaggle-000"
		case DimWorkflow:
			options.Workflow = "workflow-000"
		case DimPhase:
			options.Phase = "completed"
		case DimStage:
			options.Stage = "build"
		case DimPopulation:
			options.Population = PopulationCostMeasured
		case DimSince:
			options.Since = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		case DimUntil:
			options.Until = time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
		default:
			return ListOptions{}, false
		}
	}
	return options, true
}

// seedProbeCorpus writes runs dense enough that an unbounded walk is visible.
//
// Every run carries the `build` stage and lands in gaggle-000/workflow-000 often
// enough that each combination has far more than `limit` matches — otherwise the
// query would exhaust its candidates before reaching the limit and the bound
// would hold for the wrong reason.
func seedProbeCorpus(t *testing.T, store *Store, n int) {
	t.Helper()
	ctx := t.Context()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		startedAt := base.Add(time.Duration(i) * time.Minute)
		p := Projection{Run: RunRow{
			RunID:        fmt.Sprintf("run-%08d", i),
			Gaggle:       "gaggle-000",
			Workflow:     "workflow-000",
			Phase:        "completed",
			Terminal:     true,
			StartedAt:    startedAt,
			LastActivity: startedAt,
			LastSeq:      uint64(i + 1),
			Stages:       []string{"build"},
		}}
		if i == n/2 {
			p.Run.GooberDigest = "needle"
		}
		p.Stages = []StageRow{{
			RunID: p.Run.RunID, Stage: "build", Attempts: 1, LastStatus: "success",
			StartedAt: &startedAt,
		}}
		p.ApplyMeasurement([]StageMeasurement{{
			Stage: "build", CostMeasured: true, TokenMeasured: true,
			PremiumMeasured: true, RetryWaste: true,
		}})
		if err := store.UpsertRun(ctx, p); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := store.writeDB().Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

var _ = sql.ErrNoRows
