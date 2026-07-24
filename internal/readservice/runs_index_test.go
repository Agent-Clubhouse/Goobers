package readservice

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
)

// buildIndex opens a fresh rollup at the layout's telemetry path. With no
// names it rebuilds the whole index from disk; with names it ingests only
// those runs, leaving the index deliberately incomplete for the
// completeness/reconcile test.
func buildIndex(t *testing.T, layout instance.Layout, names ...string) *rollup.DB {
	t.Helper()
	if len(names) == 0 {
		if err := rollup.RebuildAll(layout.TelemetryDB(), []string{layout.RunsDir()}, layout.SchedulerDir()); err != nil {
			t.Fatalf("rebuild index: %v", err)
		}
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, name := range names {
		if err := db.IngestRun(filepath.Join(layout.RunsDir(), name)); err != nil {
			t.Fatalf("ingest %s: %v", name, err)
		}
	}
	return db
}

// indexedAndScanning returns two services over the same on-disk runs: one with
// the telemetry index attached (the DASH-18 path) and one without (the
// journal-scanning fallback). db governs which runs the index knows about.
func indexedAndScanning(t *testing.T, layout instance.Layout, db *rollup.DB) (indexed, scanning *Local) {
	t.Helper()
	var err error
	indexed, err = NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions(), Telemetry: db}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	scanning, err = NewLocal(LocalSources{Layout: layout, Definitions: testDefinitions()}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return indexed, scanning
}

// seedVariedRuns writes n runs across a spread of gaggles, triggers, phases,
// and start times so ordering, tie-breaking, and every filter get exercised.
func seedVariedRuns(t *testing.T, layout instance.Layout, machine *workflow.Machine, n int) {
	t.Helper()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	gaggles := []string{"goobers", "acme-web", "widget-service"}
	triggers := []journal.Trigger{{Kind: journal.TriggerManual}, {Kind: journal.TriggerSchedule}}
	phases := []journal.RunPhase{journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseEscalated}
	for i := 0; i < n; i++ {
		// Deliberately collide some start times to exercise the run_id tiebreak.
		started := base.Add(time.Duration(i/2) * time.Minute)
		run, clock := createFixtureRun(
			t, layout, machine,
			fmt.Sprintf("run-%04d", i), machine.Def.Name, gaggles[i%len(gaggles)],
			started, triggers[i%len(triggers)], false,
		)
		if i%5 == 0 {
			// Leave every fifth run in flight (no run.finished event).
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		appendFixtureStageAttempt(t, run, clock, "success")
		finishFixtureRun(t, run, clock, phases[i%len(phases)])
	}
}

func runIDs(list RunList) []string {
	ids := make([]string, 0, len(list.Runs))
	for _, r := range list.Runs {
		ids = append(ids, r.ID)
	}
	return ids
}

// listAllPages walks the cursor to the end and returns every run id in order,
// so pagination is verified end-to-end, not just the first page.
func listAllPages(t *testing.T, s *Local, options RunListOptions) []string {
	t.Helper()
	var ids []string
	seen := map[string]bool{}
	for {
		page, err := s.ListRuns(context.Background(), options)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		for _, r := range page.Runs {
			if seen[r.ID] {
				t.Fatalf("duplicate run %q across pages", r.ID)
			}
			seen[r.ID] = true
			ids = append(ids, r.ID)
		}
		if page.NextCursor == "" {
			return ids
		}
		options.Cursor = page.NextCursor
	}
}

// TestListRunsReconcileCollapsesConcurrentBurst proves the fan-out fix: the
// Overview fires one ListRuns per phase concurrently, and each formerly ran a
// full run-directory reconcile scan. Under a fixed clock the whole burst must
// collapse to a single scan.
func TestListRunsReconcileCollapsesConcurrentBurst(t *testing.T) {
	_, layout, machine := fixtureService(t)
	seedVariedRuns(t, layout, machine, 20)
	indexed, _ := indexedAndScanning(t, layout, buildIndex(t, layout))
	fixed := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	indexed.now = func() time.Time { return fixed }

	var scans atomic.Int64
	reconcileScanObserver = func() { scans.Add(1) }
	t.Cleanup(func() { reconcileScanObserver = nil })

	const concurrency = 8
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			if _, err := indexed.ListRuns(context.Background(), RunListOptions{Limit: 5}); err != nil {
				t.Errorf("ListRuns: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := scans.Load(); got != 1 {
		t.Fatalf("concurrent ListRuns burst ran %d reconcile scans, want 1", got)
	}
}

// TestListRunsReconcileRefreshesAfterInterval proves the throttle is a window,
// not a one-shot: within reconcileInterval a repeat list reuses the prior scan,
// but once the window elapses the next list scans again so imported/migrated
// runs are still eventually reconciled.
func TestListRunsReconcileRefreshesAfterInterval(t *testing.T) {
	_, layout, machine := fixtureService(t)
	seedVariedRuns(t, layout, machine, 12)
	indexed, _ := indexedAndScanning(t, layout, buildIndex(t, layout))
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	nowVal := base
	indexed.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return nowVal
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		nowVal = nowVal.Add(d)
	}

	var scans atomic.Int64
	reconcileScanObserver = func() { scans.Add(1) }
	t.Cleanup(func() { reconcileScanObserver = nil })

	list := func() {
		if _, err := indexed.ListRuns(context.Background(), RunListOptions{Limit: 5}); err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
	}

	list() // first ever list: scans (lastReconcile was zero)
	if got := scans.Load(); got != 1 {
		t.Fatalf("first list ran %d scans, want 1", got)
	}
	advance(reconcileInterval - time.Millisecond)
	list() // within the window: throttled
	if got := scans.Load(); got != 1 {
		t.Fatalf("in-window list ran %d scans total, want 1", got)
	}
	advance(2 * time.Millisecond) // now past the window
	list()
	if got := scans.Load(); got != 2 {
		t.Fatalf("post-window list ran %d scans total, want 2", got)
	}
}

func TestListRunsIndexedMatchesScanningAcrossFilters(t *testing.T) {
	_, layout, machine := fixtureService(t)
	seedVariedRuns(t, layout, machine, 37)
	indexed, scanning := indexedAndScanning(t, layout, buildIndex(t, layout))

	cases := []RunListOptions{
		{Limit: 5},                                                   // paginated, no filter
		{Limit: 200},                                                 // single page
		{Gaggle: "acme-web", Limit: 4},                               // gaggle filter
		{Workflow: machine.Def.Name, Limit: 6},                       // workflow filter
		{Trigger: journal.TriggerSchedule, Limit: 3},                 // trigger filter (index-pushed)
		{Phase: journal.PhaseFailed, Limit: 3},                       // phase filter (journal-applied)
		{Phase: journal.PhaseCompleted, Gaggle: "goobers", Limit: 2}, // mixed
		{Stage: "implement", Outcome: OutcomeSuccess, Limit: 3},      // stage/outcome (journal-applied)
		{Since: time.Date(2026, 7, 1, 12, 5, 0, 0, time.UTC), Limit: 4},
	}
	for i, opts := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			gotIndexed := listAllPages(t, indexed, opts)
			gotScanning := listAllPages(t, scanning, opts)
			if fmt.Sprint(gotIndexed) != fmt.Sprint(gotScanning) {
				t.Fatalf("indexed vs scanning diverge for %+v:\n indexed=%v\nscanning=%v", opts, gotIndexed, gotScanning)
			}
		})
	}
}

// TestListRunsIndexedBackfillsUnindexedRuns is the reviewer's exact concern:
// a run present on disk but missing from the index (migrated / imported / not
// yet ingested) must never be silently hidden. The index here holds only three
// of the seeded runs; the list must still return all of them, byte-identical
// to the scanning path.
func TestListRunsIndexedBackfillsUnindexedRuns(t *testing.T) {
	_, layout, machine := fixtureService(t)
	seedVariedRuns(t, layout, machine, 12)
	// Deliberately ingest only a sparse subset — simulate a partial/migrated index.
	partial := buildIndex(t, layout, "run-0001", "run-0007", "run-0010")
	indexed, scanning := indexedAndScanning(t, layout, partial)

	gotIndexed := listAllPages(t, indexed, RunListOptions{Limit: 5})
	gotScanning := listAllPages(t, scanning, RunListOptions{Limit: 5})
	if len(gotIndexed) != 12 {
		t.Fatalf("indexed returned %d runs, want all 12 (reconcile must backfill the index)", len(gotIndexed))
	}
	if fmt.Sprint(gotIndexed) != fmt.Sprint(gotScanning) {
		t.Fatalf("after reconcile, indexed != scanning:\n indexed=%v\nscanning=%v", gotIndexed, gotScanning)
	}
}

// TestListRunsIndexedReadsAreBoundedByPage proves the perf claim
// deterministically (no wall-clock): listing one page opens a number of
// journals bounded by page size, not by the total run count. The scanning path
// opens every run; the indexed path opens ~limit.
func TestListRunsIndexedReadsAreBoundedByPage(t *testing.T) {
	_, layout, machine := fixtureService(t)
	const total = 150
	seedVariedRuns(t, layout, machine, total)
	indexed, _ := indexedAndScanning(t, layout, buildIndex(t, layout))

	var opened int
	openRunObserver = func(string) { opened++ }
	t.Cleanup(func() { openRunObserver = nil })

	page, err := indexed.ListRuns(context.Background(), RunListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 10 {
		t.Fatalf("page returned %d runs, want 10", len(page.Runs))
	}
	// One unfiltered page of 10 must not open anywhere near all 300 journals.
	// Allow generous slack for the 100-row index fetch window, but it must be
	// bounded by page size — decisively less than total.
	if opened > 60 {
		t.Fatalf("indexed page opened %d journals for %d runs; expected bounded by page size, not O(total)", opened, total)
	}
	_ = runIDs(page)
}
