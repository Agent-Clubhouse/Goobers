package readmodel

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// TestLatestPerWorkflowPicksTheNewestRun pins correctness before anything about
// cost. An aggregate that is fast and wrong is worse than the slow one.
func TestLatestPerWorkflowPicksTheNewestRun(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Three workflows, forty runs each. wf-1's newest is still in flight, so the
	// aggregate must report a non-terminal latest AND an active count.
	for w := 0; w < 3; w++ {
		for i := 0; i < 40; i++ {
			phase := journal.PhaseCompleted
			if w == 1 && i == 39 {
				phase = journal.PhaseRunning
			}
			row := RunRow{
				RunID:     fmt.Sprintf("%032x", w*100+i),
				Gaggle:    "alpha",
				Workflow:  fmt.Sprintf("wf-%d", w),
				Phase:     phase,
				Terminal:  phase != journal.PhaseRunning,
				StartedAt: base.Add(time.Duration(w*100+i) * time.Minute),
				LastSeq:   uint64(i + 1),
			}
			if row.Terminal {
				finished := row.StartedAt.Add(time.Minute)
				row.FinishedAt = &finished
			}
			if err := store.UpsertRun(ctx, Projection{Run: row}); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}

	got, err := store.LatestPerWorkflow(ctx, AggregateOptions{Gaggle: "alpha"})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d workflows, want 3", len(got))
	}
	for _, item := range got {
		wantNewest := base.Add(time.Duration(workflowIndex(item.Run.Workflow)*100+39) * time.Minute)
		if !item.Run.StartedAt.Equal(wantNewest) {
			t.Errorf("%s latest started_at = %s, want %s (the newest run, not an arbitrary one)",
				item.Run.Workflow, item.Run.StartedAt, wantNewest)
		}
	}

	// The in-flight workflow reports non-terminal and an active count of one;
	// the others report zero. That count is what the concurrency ceiling is
	// compared against, so getting it from the same aggregate is the point.
	for _, item := range got {
		switch item.Run.Workflow {
		case "wf-1":
			if item.Run.Terminal || item.Run.Phase != journal.PhaseRunning {
				t.Errorf("wf-1 latest = %s/terminal=%v, want running/false", item.Run.Phase, item.Run.Terminal)
			}
			if item.ActiveRuns != 1 {
				t.Errorf("wf-1 active runs = %d, want 1", item.ActiveRuns)
			}
			if item.Run.FinishedAt != nil {
				t.Errorf("wf-1 latest has finished_at %v while running", item.Run.FinishedAt)
			}
		default:
			if item.ActiveRuns != 0 {
				t.Errorf("%s active runs = %d, want 0", item.Run.Workflow, item.ActiveRuns)
			}
		}
	}
}

// TestLatestPerWorkflowAgreesWithTheList pins that the aggregate and the list
// name the SAME run as latest.
//
// They resolve ties differently if you let them: the list orders
// (started_at DESC, run_id ASC), so the aggregate breaks ties with MIN(run_id)
// to match. Disagreeing would make the Workflows page and the Runs page tell
// different stories about the same workflow, which is worse than either being
// wrong on its own.
func TestLatestPerWorkflowAgreesWithTheList(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	at := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)

	// Two runs of the same workflow sharing a started_at — the tie case.
	for i, id := range []string{fmt.Sprintf("%032x", 2), fmt.Sprintf("%032x", 1)} {
		row := RunRow{
			RunID: id, Gaggle: "alpha", Workflow: "wf-tie",
			Phase: journal.PhaseCompleted, Terminal: true,
			StartedAt: at, LastSeq: uint64(i + 1),
		}
		finished := at.Add(time.Minute)
		row.FinishedAt = &finished
		if err := store.UpsertRun(ctx, Projection{Run: row}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	page, err := store.ListRuns(ctx, ListOptions{Gaggle: "alpha", Workflow: "wf-tie", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Runs) == 0 {
		t.Fatal("list returned nothing")
	}
	listLatest := page.Runs[0].RunID

	aggregate, err := store.LatestPerWorkflow(ctx, AggregateOptions{Gaggle: "alpha"})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(aggregate) != 1 {
		t.Fatalf("got %d workflows, want 1", len(aggregate))
	}
	if aggregate[0].Run.RunID != listLatest {
		t.Errorf("aggregate says the latest run is %s but the list says %s; the Workflows page and "+
			"the Runs page would tell different stories about the same workflow",
			aggregate[0].Run.RunID, listLatest)
	}
}

// TestLatestPerWorkflowScalesBetterThanTheWindowFunction is the cost claim.
//
// §5.2 names rollup's LatestWorkflowRunRefs as "an unindexed window function over
// all history". The window function must rank every row in the partition before
// discarding all but the first; the aggregate groups through the ordering index
// and touches one row per workflow.
//
// # Two things this test learned the hard way
//
// FIRST: the plan cannot carry this claim. The obvious structural assertion —
// "the window function SCANs where the aggregate SEARCHes" — is false. Both
// reach the table through the SAME index:
//
//	window:    SEARCH run USING COVERING INDEX idx_run_gaggle_workflow_recency (gaggle=?)
//	           | SCAN (subquery-3) | SCAN (subquery-1)
//	aggregate: SEARCH run USING COVERING INDEX idx_run_gaggle_workflow_recency (gaggle=?)
//	           | SCAN n | SCAN p
//
// Both plans then say SCAN. The difference is in WHAT is scanned — the window
// function's subquery holds every run in the gaggle, the aggregate's CTEs hold
// one row per workflow — and the plan text does not distinguish those. This is
// the same limitation §5.7 records for residual predicates, arriving from a
// different direction: `EXPLAIN QUERY PLAN` describes access methods, not
// cardinalities.
//
// SECOND: the small corpus cannot carry it either. An earlier version asserted
// both corpora were faster and failed on a macOS runner with:
//
//	small corpus: aggregate 2.949ms vs window 1.959ms (0.66x)
//	large corpus: aggregate 2.649ms vs window 8.012ms (3.02x)
//
// The small figure is self-refuting — the LARGER corpus ran faster than the
// smaller one, so 2.949ms was startup and cache cost rather than query cost. At
// 900 rows the query is too cheap to measure against a contended host.
//
// So the assertion is made where the signal is: the LARGE corpus, with a floor
// far below every observed value, and each side estimated by its FASTEST
// repetition rather than its mean (#4087) so a loaded host cannot inflate it. Measured ratios across hosts: 5.7x, 4.1x,
// 4.0x, and 3.0x on the loaded macOS runner. A floor of 1.5x has real headroom
// while still catching the regression that matters — if the aggregate ever
// degenerated into ranking all history, the ratio would collapse toward 1.
//
// Honest scope: the aggregate is bounded by runs-IN-THE-GAGGLE, not by workflow
// count. A strictly O(workflows) shape would drive per-workflow seeks from the
// definitions inventory. What is established is that the advantage GROWS with
// history, which is the property that matters for a store that only gets bigger.
func TestLatestPerWorkflowScalesBetterThanTheWindowFunction(t *testing.T) {
	ctx := context.Background()

	small := aggregateTiming(t, ctx, 30, 30)
	large := aggregateTiming(t, ctx, 30, 150)

	// Logged, not asserted: at this size the measurement is dominated by cache
	// and startup effects rather than by the query.
	t.Logf("small corpus (900 rows, NOT asserted): aggregate %s vs window %s (%.2fx)",
		small.aggregate, small.window, small.ratio())
	t.Logf("large corpus (4500 rows): aggregate %s vs window %s (%.2fx)",
		large.aggregate, large.window, large.ratio())

	const floor = 1.5
	if large.ratio() < floor {
		t.Errorf("on the larger corpus the aggregate was only %.2fx the window function, below "+
			"the %.1fx floor; observed values across hosts are 3.0x-5.7x, so this suggests the "+
			"aggregate has degenerated into ranking all history rather than grouping through "+
			"the index", large.ratio(), floor)
	}
	if large.ratio() <= small.ratio() {
		t.Logf("note: the advantage did not grow from the small corpus to the large one "+
			"(%.2fx -> %.2fx); on a contended host that is noise, but a persistent inversion "+
			"would mean the aggregate scales no better than what it replaced",
			small.ratio(), large.ratio())
	}
}

// windowFunctionQuery is the shape rollup uses today, kept here so the
// comparison is against the real thing rather than a paraphrase of it.
const windowFunctionQuery = `SELECT run_id FROM (
	SELECT run_id, ROW_NUMBER() OVER (PARTITION BY gaggle, workflow ORDER BY started_at DESC, run_id ASC) AS rk
	FROM run WHERE gaggle = ?) WHERE rk = 1`

type aggregateTimings struct{ aggregate, window time.Duration }

func (a aggregateTimings) ratio() float64 { return float64(a.window) / float64(a.aggregate) }

func aggregateTiming(t *testing.T, ctx context.Context, workflows, runsEach int) aggregateTimings {
	t.Helper()
	store := openTestStore(t)
	seedAggregateCorpus(t, store, workflows, runsEach)

	// #4087: the MINIMUM over repetitions, not the mean. Host contention can
	// only ever ADD time to a measurement, never remove it, so the minimum
	// converges on the query's own cost while the mean converges on that cost
	// plus the runner's average interference. One preempted repetition out of
	// fifteen moves the mean and not the minimum — and since the two sides are
	// timed in separate loops at different moments, a burst landing in one and
	// not the other tilts the ratio directly. That failed PR #4076, which
	// touched nothing in this package, at 1.24x against a 1.5x floor.
	const reps = 15
	fastest := func(once func()) time.Duration {
		best := time.Duration(math.MaxInt64)
		for i := 0; i < reps; i++ {
			start := time.Now()
			once()
			if elapsed := time.Since(start); elapsed < best {
				best = elapsed
			}
		}
		return best
	}

	agg := fastest(func() {
		if _, err := store.LatestPerWorkflow(ctx, AggregateOptions{Gaggle: "alpha"}); err != nil {
			t.Fatal(err)
		}
	})
	window := fastest(func() {
		rows, err := store.reader.Query(windowFunctionQuery, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() { //nolint:revive // draining the result is the measurement
		}
		_ = rows.Close()
	})
	return aggregateTimings{aggregate: agg, window: window}
}

// seedAggregateCorpus inserts workflows x runsEach terminal runs into one gaggle.
func seedAggregateCorpus(t *testing.T, store *Store, workflows, runsEach int) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tx, err := store.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for w := 0; w < workflows; w++ {
		for i := 0; i < runsEach; i++ {
			n++
			if _, err := tx.Exec(`INSERT INTO run (run_id, gaggle, workflow, phase, terminal, started_at, last_seq)
				VALUES (?, 'alpha', ?, 'completed', 1, ?, ?)`,
				fmt.Sprintf("%032x", n), fmt.Sprintf("wf-%03d", w),
				base.Add(time.Duration(n)*time.Minute).UTC().Format(timeFormat), n); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writer.Exec("ANALYZE"); err != nil {
		t.Fatal(err)
	}
}

// TestLatestPerWorkflowSeeksThroughTheOrderingIndex pins that the inner lookup
// uses the covering index rather than scanning the table.
func TestLatestPerWorkflowSeeksThroughTheOrderingIndex(t *testing.T) {
	store := openTestStore(t)
	seedProjectedRuns(t, store, 300)

	query, args := latestPerWorkflowQuery(AggregateOptions{Gaggle: "gaggle-000"})
	plan := explainWithArgs(t, store, query, args)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "idx_run_gaggle_workflow_recency") {
		t.Errorf("the per-workflow lookup does not use the ordering index:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN run\n") || strings.Contains(plan, "SCAN run |") {
		t.Errorf("the aggregate performs a bare table scan:\n%s", plan)
	}
}

func workflowIndex(name string) int {
	var i int
	_, _ = fmt.Sscanf(name, "wf-%d", &i)
	return i
}

// TestLatestPerWorkflowTerminalOnlySkipsTheRunningRun pins the distinction the
// Workflows page depends on: "what did this workflow last do" is not "what is it
// doing".
//
// A workflow whose newest run is in flight must still report its last OUTCOME,
// alongside a live active count. Reporting the running run as the latest outcome
// would blank the page's result column exactly when someone is watching it.
func TestLatestPerWorkflowTerminalOnlySkipsTheRunningRun(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	seed := func(id string, offset time.Duration, phase journal.RunPhase) {
		t.Helper()
		row := RunRow{
			RunID: id, Gaggle: "alpha", Workflow: "wf",
			Phase: phase, Terminal: phase != journal.PhaseRunning,
			StartedAt: base.Add(offset), LastSeq: 1,
		}
		if row.Terminal {
			finished := row.StartedAt.Add(time.Minute)
			row.FinishedAt = &finished
		}
		if err := store.UpsertRun(ctx, Projection{Run: row}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed(fmt.Sprintf("%032x", 1), 0, journal.PhaseCompleted)
	seed(fmt.Sprintf("%032x", 2), time.Hour, journal.PhaseFailed)
	seed(fmt.Sprintf("%032x", 3), 2*time.Hour, journal.PhaseRunning)

	latest, err := store.LatestPerWorkflow(ctx, AggregateOptions{Gaggle: "alpha", TerminalOnly: true})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("got %d workflows, want 1", len(latest))
	}
	if got := latest[0].Run.RunID; got != fmt.Sprintf("%032x", 2) {
		t.Errorf("latest terminal run = %s, want the failed run at +1h; the running run is newer "+
			"but is not an outcome", got)
	}
	if latest[0].Run.Phase != journal.PhaseFailed {
		t.Errorf("latest terminal phase = %s, want failed", latest[0].Run.Phase)
	}
	// The active count is NOT filtered by terminality — it is a different
	// question about the same workflow, and both have to be answerable at once.
	if latest[0].ActiveRuns != 1 {
		t.Errorf("active runs = %d, want 1; the in-flight run still has to be counted",
			latest[0].ActiveRuns)
	}

	// Without TerminalOnly the same store reports the running run, so the two
	// modes are demonstrably different rather than one being dead configuration.
	anyLatest, err := store.LatestPerWorkflow(ctx, AggregateOptions{Gaggle: "alpha"})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(anyLatest) != 1 || anyLatest[0].Run.RunID != fmt.Sprintf("%032x", 3) {
		t.Errorf("unrestricted latest = %+v, want the running run", anyLatest)
	}
}

// TestLatestTerminalTieDoesNotPickARunningRun constructs the timestamp tie the
// pick join exists for.
//
// Two runs of one workflow start at the same instant; one finishes, one is still
// going. The terminal-only aggregate must return the finished one regardless of
// which run_id sorts first — if the join did not repeat the terminality test,
// MIN(run_id) could hand back the running run.
func TestLatestTerminalTieDoesNotPickARunningRun(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	// The RUNNING run gets the LOWER run_id, so MIN(run_id) prefers it.
	running := RunRow{
		RunID: fmt.Sprintf("%032x", 1), Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseRunning, Terminal: false, StartedAt: at, LastSeq: 1,
	}
	finished := at.Add(time.Minute)
	completed := RunRow{
		RunID: fmt.Sprintf("%032x", 2), Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseCompleted, Terminal: true, StartedAt: at,
		FinishedAt: &finished, LastSeq: 1,
	}
	for _, row := range []RunRow{running, completed} {
		if err := store.UpsertRun(ctx, Projection{Run: row}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	latest, err := store.LatestPerWorkflow(ctx, AggregateOptions{Gaggle: "alpha", TerminalOnly: true})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("got %d workflows, want 1", len(latest))
	}
	if latest[0].Run.RunID != completed.RunID {
		t.Errorf("tie picked run %s (phase %s), want the completed run %s; the pick join must "+
			"repeat the terminality test", latest[0].Run.RunID, latest[0].Run.Phase, completed.RunID)
	}
}

// TestLatestPerWorkflowUsesThePartialTerminalIndex pins that terminality is
// served by the partial index rather than evaluated as a residual predicate.
//
// §5.7: a plan cannot prove a residual predicate is ABSENT, so the check is the
// positive one — the partial index has to appear by name. It is cheap today
// because nearly every run is terminal; the index is what keeps that a property
// of the schema rather than of the corpus.
func TestLatestPerWorkflowUsesThePartialTerminalIndex(t *testing.T) {
	store := openTestStore(t)
	seedProjectedRuns(t, store, 400)

	query, args := latestPerWorkflowQuery(AggregateOptions{Gaggle: "gaggle-000", TerminalOnly: true})
	plan := explainWithArgs(t, store, query, args)
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "idx_run_terminal_gaggle_workflow_recency") {
		t.Errorf("terminality is a residual predicate rather than an indexed one; the partial "+
			"index does not appear in the plan:\n%s", plan)
	}
}
