package readmodel

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
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
			rows, err := store.reader.Query(instrumented, args...)
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

func TestActiveRunCountWorkDoesNotGrowWithCompletedHistory(t *testing.T) {
	registerRowProbe(t)
	store := openTestStore(t)
	started := time.Now().UTC().Format(timeFormat)

	insert := func(id, phase string) {
		t.Helper()
		if _, err := store.writer.Exec(`
			INSERT INTO run (run_id, gaggle, workflow, phase, terminal, started_at)
			VALUES (?, 'gaggle', 'workflow', ?, ?, ?)`,
			id, phase, boolToInt(phase != "running"), started); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("active-1", "running")
	insert("active-2", "running")

	visited := func() int64 {
		t.Helper()
		probeCount.Store(0)
		rows, err := store.reader.Query(`
			SELECT gaggle, workflow, COUNT(*)
			FROM run
			WHERE phase = 'running' AND probe(run_id) = 1
			GROUP BY gaggle, workflow`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		return probeCount.Load()
	}

	before := visited()
	for i := range 5_000 {
		insert(fmt.Sprintf("completed-%05d", i), "completed")
	}
	after := visited()
	if before != 2 || after != before {
		t.Fatalf("rows visited before=%d after=%d; completed retention increased active-count work", before, after)
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
	rows, err := store.reader.Query(query)
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
		case DimOutcome:
			options.Outcome = OutcomeSuccess
		case DimPopulation:
			options.Population = PopulationCostMeasured
		case DimActivity:
			options.OrderBy = OrderLastActivity
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
			StartedAt: &startedAt, HadSuccess: true,
		}}
		p.ApplyMeasurement([]StageMeasurement{{
			Stage: "build", CostMeasured: true, TokenMeasured: true,
			PremiumMeasured: true, RetryWaste: true,
		}})
		if err := store.UpsertRun(ctx, p); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := store.writer.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// TestBoundHoldsAtOneHundredThousandRows is §14.2's stated scale.
//
// # Why the bound has to be re-measured at scale rather than extrapolated
//
// A bound that holds at 3,000 rows can break at 100,000. SQLite's planner
// chooses from statistics, and a plan that seeks a covering index on a small
// table can switch to a scan-and-filter on a large one — at which point the
// query still returns limit+1 rows and starts examining the whole table. That
// is precisely the failure §5.7 exists to prevent, and it is invisible to every
// check except this one.
//
// # Why an env var rather than -short
//
// Seeding 100,000 rows costs ~109 s, which does not belong in a gate that was
// deliberately parallelised down to 4-6 minutes. `-short` would not do it:
// test/ci does not pass that flag, so a `testing.Short()` guard would skip for
// developers and run on every commit — exactly backwards.
//
// The per-commit protection is TestEveryCombinationVisitsAtMostLimitPlusOneRows
// at 3,000 rows, which catches a regression in the query or the index set. This
// test catches something different and rarer: the planner switching strategy as
// statistics grow. That is worth checking deliberately, not continuously.
//
// Run it with:
//
//	GOOBERS_SCALE_TESTS=1 go test ./internal/readmodel/ \
//		-run TestBoundHoldsAtOneHundredThousandRows -timeout 30m
//
// Recorded result: 100k rows, 44 combinations, worst case 51 rows visited at
// limit 50.
func TestBoundHoldsAtOneHundredThousandRows(t *testing.T) {
	if os.Getenv("GOOBERS_SCALE_TESTS") == "" {
		t.Skip("set GOOBERS_SCALE_TESTS=1 to run; seeds 100k rows (~109s)")
	}
	registerRowProbe(t)

	store := openTestStore(t)
	const (
		rows         = 100_000
		stagesPerRun = 10 // 1,000,000 run_stage rows — §14.2's "1M+ attempts"
	)
	seedProbeCorpusBulk(t, store, rows, stagesPerRun)

	const limit = 50
	worst := int64(0)
	var worstAt string
	for _, combination := range SupportedCombinations() {
		options, ok := probeOptionsFor(combination.Dims, limit)
		if !ok {
			t.Errorf("harness cannot express {%s}", Key(combination.Dims))
			continue
		}
		query, args := listQuery(options, limit)
		instrumented, ok := instrument(query)
		if !ok {
			t.Fatalf("could not instrument: %s", query)
		}
		if production, measured := explainWithArgs(t, store, query, args),
			explainWithArgs(t, store, instrumented, args); production != measured {
			t.Fatalf("instrumenting changed the plan for {%s}", Key(combination.Dims))
		}

		probeCount.Store(0)
		result, err := store.reader.Query(instrumented, args...)
		if err != nil {
			t.Fatalf("query {%s}: %v", Key(combination.Dims), err)
		}
		for result.Next() {
		}
		_ = result.Close()

		visited := probeCount.Load()
		if visited > worst {
			worst, worstAt = visited, Key(combination.Dims)
		}
		if visited > int64(limit+1) {
			t.Errorf("{%s} visited %d rows of %d at limit %d; the bound that held at 3k "+
				"does not hold at 100k — the planner has switched to scan-and-filter",
				Key(combination.Dims), visited, rows, limit)
		}
	}
	var stageRows int
	if err := store.reader.QueryRow(`SELECT COUNT(*) FROM run_stage`).Scan(&stageRows); err != nil {
		t.Fatalf("count run_stage: %v", err)
	}
	if stageRows < 1_000_000 {
		t.Errorf("run_stage has %d rows; §14.2 states the bound at 1M+ attempts and this "+
			"fixture does not reach it", stageRows)
	}
	t.Logf("%d runs / %d attempts, %d combinations: worst case %d rows visited at limit %d (in {%s})",
		rows, stageRows, len(SupportedCombinations()), worst, limit, worstAt)
}

// seedProbeCorpusBulk writes n projected runs in batched transactions.
//
// Direct SQL rather than UpsertRun: that path opens a transaction per run, which
// is correct for projection and far too slow for a 100,000-row fixture. The rows
// it writes are the same shape — this is a fixture builder, not a second
// implementation of projection, and the per-combination queries it feeds only
// read columns that are set here.
func seedProbeCorpusBulk(t *testing.T, store *Store, n, stagesPerRun int) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const batch = 5_000

	for start := 0; start < n; start += batch {
		tx, err := store.writer.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		runStmt, err := tx.Prepare(`INSERT INTO run (
			run_id, gaggle, workflow, phase, terminal, started_at, last_activity_at, last_seq,
			repass_count, retry_count, policy_retry_count, infra_retry_count,
			any_token_measured, any_premium_measured, any_cost_measured, any_retry_waste
		) VALUES (?, ?, ?, 'completed', 1, ?, ?, ?, 0, 0, 0, 0, 1, 1, 1, 1)`)
		if err != nil {
			t.Fatalf("prepare run: %v", err)
		}
		stageStmt, err := tx.Prepare(`INSERT INTO run_stage (
			run_id, stage, attempts, last_status, gaggle, run_started_at,
			token_measured, premium_measured, cost_measured, retry_waste
		) VALUES (?, ?, ?, 'success', ?, ?, 1, 1, 1, 1)`)
		if err != nil {
			t.Fatalf("prepare stage: %v", err)
		}

		end := start + batch
		if end > n {
			end = n
		}
		for i := start; i < end; i++ {
			id := fmt.Sprintf("run-%08d", i)
			at := formatTime(base.Add(time.Duration(i) * time.Minute))
			if _, err := runStmt.Exec(id, "gaggle-000", "workflow-000", at, at, i+1); err != nil {
				t.Fatalf("insert run %d: %v", i, err)
			}
			// stagesPerRun rows per run, so run_stage reaches 1M+ at 100k runs.
			//
			// This is the half of §14.2's "100k runs / 1M+ attempts" that a single
			// stage per run does not test. Stage-scoped queries drive FROM
			// run_stage, so its row count is what bounds them — and with one stage
			// per run the index a `stage=build` query seeks contains only matching
			// rows, which is the easy case. With ten stages it contains ten times
			// the data and one tenth of it matches, which is the shape a real
			// instance has.
			for stage := 0; stage < stagesPerRun; stage++ {
				name := "build"
				if stage > 0 {
					name = fmt.Sprintf("stage-%02d", stage)
				}
				if _, err := stageStmt.Exec(id, name, stage+1, "gaggle-000", at); err != nil {
					t.Fatalf("insert stage %d/%d: %v", i, stage, err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit batch at %d: %v", start, err)
		}
	}
	// ANALYZE after loading, not before: the planner's choices are what this test
	// is about, and it must make them against the statistics a real 100k store
	// would have.
	if _, err := store.writer.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

var _ = sql.ErrNoRows
