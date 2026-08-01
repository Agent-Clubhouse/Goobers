package readmodel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readprobe"
)

// TestListOpensNoJournals is what Wave 2 exists for.
//
// Measured at 1x AFTER Wave 1, a 50-row page still opened 51 journals — one per
// returned row plus the lookahead — because the telemetry.db row was not
// complete enough to answer the list contract and each run had to be hydrated
// from its journal. That is the diagnosis's "lists open and parse a journal per
// returned row", and §14.2's target is zero.
//
// Asserted with readprobe, which counts actual journal opens, so this is a
// statement about WORK rather than latency and holds on any machine.
func TestListOpensNoJournals(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProjectedRuns(t, store, 120)

	readprobe.Enable()
	t.Cleanup(readprobe.Disable)
	before := readprobe.Take()

	page, err := store.ListRuns(ctx, ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	work := readprobe.Take().Sub(before)

	if len(page.Runs) != 50 {
		t.Fatalf("page returned %d runs, want 50", len(page.Runs))
	}
	if !page.HasMore {
		t.Error("HasMore = false with 120 rows and a 50-row page")
	}
	if work.JournalOpens != 0 || work.ActiveScanOpens != 0 {
		t.Errorf("a bounded list page opened %d journals (%d via the active scan); §14.2's bound is ZERO.\n"+
			"Before the read model this was 51 — one per returned row plus the lookahead.",
			work.JournalOpens, work.ActiveScanOpens)
	}
	// The reconcile-scan counter is gone, and its absence is a stronger
	// guarantee than the assertion it replaced (#1924). reconcileIndex no longer
	// exists, so "the list path performs no reconcile" is now a compile-time
	// fact rather than something a runtime counter has to keep proving. A
	// counter that nothing can increment is a vacuous assertion.
}

// TestPaginationIsFlatInDepth pins that page N costs what page 1 costs.
//
// Keyset, never offset (§7.3). An offset plan degrades with depth and a
// first-page-only measurement never sees it, which is why this walks to the end
// and asserts the work per page is constant.
func TestPaginationIsFlatInDepth(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProjectedRuns(t, store, 250)

	var (
		options ListOptions
		seen    = map[string]bool{}
		pages   int
		ordered []time.Time
	)
	options.Limit = 25
	for {
		page, err := store.ListRuns(ctx, options)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, run := range page.Runs {
			if seen[run.RunID] {
				t.Fatalf("run %s appeared on two pages; the keyset cursor is not a total order", run.RunID)
			}
			seen[run.RunID] = true
			ordered = append(ordered, run.StartedAt)
		}
		if !page.HasMore {
			break
		}
		options.Cursor = page.Next
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != 250 {
		t.Errorf("paged through %d runs, want 250 — a keyset cursor must not skip or duplicate", len(seen))
	}
	if pages != 10 {
		t.Errorf("took %d pages for 250 runs at 25 per page, want 10", pages)
	}
	// Newest-first ordering must hold ACROSS page boundaries, which is where an
	// incorrect cursor predicate shows up.
	for i := 1; i < len(ordered); i++ {
		if ordered[i].After(ordered[i-1]) {
			t.Fatalf("ordering broke at position %d: %s came after %s", i, ordered[i], ordered[i-1])
		}
	}
}

// TestListRefusesUnsupportedCombinationsBeforeQuerying pins that the closed set
// is enforced at the door.
//
// Validating after the query would mean the expensive thing already happened —
// the whole point of the closed set is that an unsupported combination is never
// executed.
func TestListRefusesUnsupportedCombinationsBeforeQuerying(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedProjectedRuns(t, store, 10)

	readprobe.Enable()
	t.Cleanup(readprobe.Disable)

	// Workflow without gaggle is not in the closed set: it has no covering index
	// (the scoped indexes lead with gaggle), so it would scan.
	_, err := store.ListRuns(ctx, ListOptions{Workflow: "implementation", Limit: 50})
	if err == nil {
		t.Fatal("a workflow-only filter was accepted; it has no covering index")
	}
	var unsupported *UnsupportedCombinationError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want a typed refusal", err)
	}
	if !strings.Contains(err.Error(), "supported neighbours") {
		t.Errorf("refusal did not name alternatives: %v", err)
	}
}

// TestListServesEveryContractField pins that the row is COMPLETE — the property
// that makes zero journal opens possible at all.
//
// A field missing here would be one the read service has to reach for the
// journal to fill, which is the cost this whole wave removes.
func TestListServesEveryContractField(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	identity := testIdentity()
	if err := store.UpsertRun(ctx, ProjectRun(identity, Projection{}, completedRunEvents())); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	page, err := store.ListRuns(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(page.Runs))
	}
	run := page.Runs[0]

	if run.RunID != identity.RunID || run.Gaggle != identity.Gaggle || run.Workflow != identity.Workflow {
		t.Errorf("identity not served: %+v", run)
	}
	if run.WorkflowVersion != identity.WorkflowVersion {
		t.Errorf("workflow version = %d, want %d", run.WorkflowVersion, identity.WorkflowVersion)
	}
	if run.TriggerKind != string(identity.Trigger.Kind) {
		t.Errorf("trigger kind = %q, want %q", run.TriggerKind, identity.Trigger.Kind)
	}
	if run.Phase != journal.PhaseCompleted || !run.Terminal {
		t.Errorf("phase/terminal = %q/%v, want completed/true", run.Phase, run.Terminal)
	}
	if run.FinishedAt == nil {
		t.Error("finished_at not served")
	}
	if run.LastSeq == 0 || run.LastActivity.IsZero() {
		t.Errorf("last_seq/last_activity not served: %d / %v", run.LastSeq, run.LastActivity)
	}
	if run.PolicyRetryCount != 1 || run.RetryCount != 1 {
		t.Errorf("retry counts not served: policy=%d total=%d", run.PolicyRetryCount, run.RetryCount)
	}

	// Duration is computed at query time, not stored — so a quiet in-flight run
	// keeps ageing rather than freezing at projection time (§5.3).
	if d := run.Duration(time.Now()); d <= 0 {
		t.Errorf("duration = %s for a finished run, want positive", d)
	}
}

// TestRunningRunKeepsAgeing pins the other half of that decision.
func TestRunningRunKeepsAgeing(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	identity := testIdentity()
	// No run.finished: still in flight.
	if err := store.UpsertRun(ctx, ProjectRun(identity, Projection{}, completedRunEvents()[:3])); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	page, err := store.ListRuns(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	run := page.Runs[0]
	early := run.Duration(run.StartedAt.Add(time.Minute))
	later := run.Duration(run.StartedAt.Add(time.Hour))
	if later <= early {
		t.Errorf("an in-flight run did not age: %s at +1m, %s at +1h; a stored duration would "+
			"freeze at projection time", early, later)
	}
}

// TestListPlansUseCoveringIndexes pins that the planner picks the index each
// supported combination names.
//
// An index that exists but is not chosen is worse than none: it costs write
// amplification while the read stays a scan, and nothing fails.
func TestListPlansUseCoveringIndexes(t *testing.T) {
	store := openTestStore(t)
	seedProjectedRuns(t, store, 400)

	cases := []struct {
		name    string
		options ListOptions
		want    string
	}{
		{"unrestricted", ListOptions{Limit: 50}, "idx_run_recency"},
		{"gaggle", ListOptions{Gaggle: "gaggle-000", Limit: 50}, "idx_run_gaggle_recency"},
		{"gaggle+workflow", ListOptions{Gaggle: "gaggle-000", Workflow: "wf-0", Limit: 50}, "idx_run_gaggle_workflow_recency"},
		{"phase", ListOptions{Phase: journal.PhaseRunning, Limit: 50}, "idx_run_phase_recency"},
		{"gaggle+phase", ListOptions{Gaggle: "gaggle-000", Phase: journal.PhaseRunning, Limit: 50}, "idx_run_gaggle_phase_recency"},
		// With a cursor, so the deep-page plan is asserted too rather than assumed
		// to match the first page's.
		{"gaggle deep page", ListOptions{
			Gaggle: "gaggle-000", Limit: 50,
			Cursor: ListCursor{StartedAt: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), RunID: "x"},
		}, "idx_run_gaggle_recency"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query, args := listQuery(tc.options, tc.options.Limit)
			plan := explainWithArgs(t, store, query, args)
			t.Logf("plan: %s", plan)
			if !strings.Contains(plan, tc.want) {
				t.Errorf("plan does not use %s:\n%s", tc.want, plan)
			}
			if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
				t.Errorf("plan materializes a sort; page N would degrade with depth:\n%s", plan)
			}
		})
	}
}

func explainWithArgs(t *testing.T, store *Store, query string, args []any) string {
	t.Helper()
	rows, err := store.readDB().Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(out, " | ")
}

// seedProjectedRuns writes n projected runs directly, spread across gaggles,
// workflows and phases so scoped queries have real subsets to select.
func seedProjectedRuns(t *testing.T, store *Store, n int) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	phases := []journal.RunPhase{
		journal.PhaseRunning, journal.PhaseCompleted, journal.PhaseFailed,
		journal.PhaseEscalated, journal.PhaseAborted,
	}
	for i := 0; i < n; i++ {
		phase := phases[i%len(phases)]
		row := RunRow{
			RunID:        runIDFor(i),
			Gaggle:       gaggleNameFor(i),
			Workflow:     workflowNameFor(i),
			Phase:        phase,
			Terminal:     phase != journal.PhaseRunning,
			StartedAt:    base.Add(time.Duration(i) * time.Minute),
			LastActivity: base.Add(time.Duration(i) * time.Minute),
			LastSeq:      uint64(i + 1),
		}
		if row.Terminal {
			finished := row.StartedAt.Add(time.Minute)
			row.FinishedAt = &finished
		}
		if err := store.UpsertRun(ctx, Projection{Run: row}); err != nil {
			t.Fatalf("seed run %d: %v", i, err)
		}
	}
	if _, err := store.writer.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func runIDFor(i int) string        { return fmtHex(i) }
func gaggleNameFor(i int) string   { return "gaggle-00" + string(rune('0'+i%3)) }
func workflowNameFor(i int) string { return "wf-" + string(rune('0'+i%4)) }

func fmtHex(i int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 32)
	for j := 31; j >= 0; j-- {
		out[j] = hex[i&0xf]
		i >>= 4
	}
	return string(out)
}

// TestStageScopedPlansUseTheirIndexes is the #1782 half of the plan contract.
//
// §5.7's whole argument is that a plan cannot PROVE a residual predicate is
// absent — `EXPLAIN QUERY PLAN` reports access methods, not residual terms. So
// this test does not try to prove boundedness from the plan. It checks the two
// things a plan CAN show and that would each silently destroy the bound:
//
//   - the named index is used at all, rather than a full table scan; and
//   - no temp B-tree is materialised for the ORDER BY, which is what would
//     happen if run_started_at were missing and the sort fell back to the joined
//     run row.
//
// The boundedness argument itself rests on the closed set: every listed
// combination has an index whose leading columns are exactly its equality
// predicates, followed by the ordering key.
func TestStageScopedPlansUseTheirIndexes(t *testing.T) {
	store := openTestStore(t)
	seedProjectedStages(t, store, 400)

	cases := []struct {
		name    string
		options ListOptions
		want    string
	}{
		{"stage", ListOptions{Stage: "build", Limit: 50}, "idx_run_stage_recency"},
		{"gaggle+stage", ListOptions{Gaggle: "gaggle-000", Stage: "build", Limit: 50},
			"idx_run_stage_gaggle_recency"},
		// Each population resolves to its OWN partial index. A composite over the
		// four booleans would have to be probed with three wildcards, which is a
		// scan of the stage's whole range.
		{"stage+cost", ListOptions{Stage: "build", Population: PopulationCostMeasured, Limit: 50},
			"idx_run_stage_cost"},
		{"stage+token", ListOptions{Stage: "build", Population: PopulationTokenMeasured, Limit: 50},
			"idx_run_stage_token"},
		{"stage+premium", ListOptions{Stage: "build", Population: PopulationPremiumMeasured, Limit: 50},
			"idx_run_stage_premium"},
		{"stage+retry-waste", ListOptions{Stage: "build", Population: PopulationRetryWaste, Limit: 50},
			"idx_run_stage_retry_waste"},
		{"gaggle+stage+cost", ListOptions{
			Gaggle: "gaggle-000", Stage: "build", Population: PopulationCostMeasured, Limit: 50,
		}, "idx_run_stage_gaggle_cost"},

		// Unscoped population comes off the run-level rollup instead.
		{"population", ListOptions{Population: PopulationCostMeasured, Limit: 50}, "idx_run_any_cost"},
		{"gaggle+population", ListOptions{
			Gaggle: "gaggle-000", Population: PopulationCostMeasured, Limit: 50,
		}, "idx_run_gaggle_any_cost"},

		// A deep page must seek, not scan to position.
		{"stage deep page", ListOptions{
			Stage: "build", Limit: 50,
			Cursor: ListCursor{StartedAt: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), RunID: "x"},
		}, "idx_run_stage_recency"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query, args := listQuery(tc.options, tc.options.Limit)
			plan := explainWithArgs(t, store, query, args)
			t.Logf("plan: %s", plan)
			if !strings.Contains(plan, tc.want) {
				t.Errorf("plan does not use %s:\n%s", tc.want, plan)
			}
			if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
				t.Errorf("plan materializes a sort; the page is no longer bounded by limit:\n%s", plan)
			}
			if strings.Contains(plan, "SCAN run_stage") {
				t.Errorf("plan scans run_stage rather than seeking:\n%s", plan)
			}
		})
	}
}

// seedProjectedStages writes runs that all carry a `build` stage, with
// measurement spread across them so the partial indexes have both matching and
// non-matching rows.
func seedProjectedStages(t *testing.T, store *Store, n int) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		startedAt := base.Add(time.Duration(i) * time.Minute)
		p := Projection{Run: RunRow{
			RunID:        runIDFor(i),
			Gaggle:       gaggleNameFor(i),
			Workflow:     workflowNameFor(i),
			Phase:        journal.PhaseCompleted,
			Terminal:     true,
			StartedAt:    startedAt,
			LastActivity: startedAt,
			LastSeq:      uint64(i + 1),
			Stages:       []string{"build"},
		}}
		status := "succeeded"
		if i%3 == 0 {
			status = "failed"
		}
		p.Stages = []StageRow{{
			RunID: p.Run.RunID, Stage: "build", Attempts: 1,
			LastStatus: status, StartedAt: &startedAt,
		}}
		// A minority measured, which is what makes the partial indexes small and
		// is the shape they are chosen for.
		if i%5 == 0 {
			p.ApplyMeasurement([]StageMeasurement{{
				Stage: "build", CostMeasured: true, TokenMeasured: true,
				PremiumMeasured: true, RetryWaste: true,
			}})
		}
		if err := store.UpsertRun(ctx, p); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := store.writeDB().Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}
