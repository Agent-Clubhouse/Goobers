package readservice

import (
	"context"
	"errors"
	"fmt"
	"os"
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
		if err := rollup.RebuildAll(context.Background(), layout.TelemetryDB(), []string{layout.RunsDir()}, layout.SchedulerDir()); err != nil {
			t.Fatalf("rebuild index: %v", err)
		}
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, name := range names {
		if err := db.IngestRun(context.Background(), filepath.Join(layout.RunsDir(), name)); err != nil {
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

// TestListRunsReconcileDiscoversRunAddedAfterFirstScan proves the incremental
// scan still backfills: a run written to disk after the first reconcile (e.g. an
// imported/migrated journal) bumps its parent directory's mtime past the
// watermark, so the next reconcile past the TTL window rediscovers and ingests
// it rather than skipping it forever.
func TestListRunsReconcileDiscoversRunAddedAfterFirstScan(t *testing.T) {
	_, layout, machine := fixtureService(t)
	seedVariedRuns(t, layout, machine, 6)
	// Full index of the seeded runs: the first reconcile has nothing to backfill
	// and simply records the watermark.
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

	before := listAllPages(t, indexed, RunListOptions{Limit: 50})
	if len(before) != 6 {
		t.Fatalf("first list returned %d runs, want 6", len(before))
	}

	// Write a new, complete run to disk after the first reconcile. Creating the
	// directory bumps the parent runs-dir mtime beyond the recorded watermark.
	started := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)
	newRun, clock := createFixtureRun(
		t, layout, machine, "run-added", machine.Def.Name, "goobers",
		started, journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	appendFixtureStageAttempt(t, newRun, clock, "success")
	finishFixtureRun(t, newRun, clock, journal.PhaseCompleted)

	// Within the throttle window the new run is not yet visible.
	if got := listAllPages(t, indexed, RunListOptions{Limit: 50}); len(got) != 6 {
		t.Fatalf("in-window list returned %d runs, want 6 (reconcile throttled)", len(got))
	}

	// Past the window the incremental scan must rediscover it.
	advance(reconcileInterval + time.Millisecond)
	after := listAllPages(t, indexed, RunListOptions{Limit: 50})
	if len(after) != 7 {
		t.Fatalf("post-window list returned %d runs, want 7 (incremental reconcile must backfill the added run)", len(after))
	}
	found := false
	for _, id := range after {
		if id == "run-added" {
			found = true
		}
	}
	if !found {
		t.Fatalf("added run not discovered by incremental reconcile: %v", after)
	}
}

// TestListRunsReconcileSkipsUnchangedDirs proves the per-scan cost is bounded:
// once the watermark is recorded, a later reconcile over an unchanged run tree
// inspects no directory entries at all — the parent mtime gate skips the ReadDir
// entirely rather than re-walking every (already-indexed) run and orphan dir.
func TestListRunsReconcileSkipsUnchangedDirs(t *testing.T) {
	_, layout, machine := fixtureService(t)
	seedVariedRuns(t, layout, machine, 10)
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

	var inspected atomic.Int64
	reconcileInspectObserver = func(string) { inspected.Add(1) }
	t.Cleanup(func() { reconcileInspectObserver = nil })

	// First reconcile is a full scan (zero watermark) and inspects every entry.
	if _, err := indexed.ListRuns(context.Background(), RunListOptions{Limit: 5}); err != nil {
		t.Fatalf("first ListRuns: %v", err)
	}
	if inspected.Load() == 0 {
		t.Fatal("full first reconcile inspected no entries; expected the whole tree")
	}

	// A second reconcile past the TTL over an unchanged tree must skip every
	// parent via the watermark and inspect nothing.
	inspected.Store(0)
	advance(reconcileInterval + time.Millisecond)
	if _, err := indexed.ListRuns(context.Background(), RunListOptions{Limit: 5}); err != nil {
		t.Fatalf("second ListRuns: %v", err)
	}
	if got := inspected.Load(); got != 0 {
		t.Fatalf("incremental reconcile inspected %d entries over an unchanged tree, want 0", got)
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
		{Phase: journal.PhaseFailed, Limit: 3},                       // phase filter (index-pushed + journal-verified)
		{Phase: journal.PhaseCompleted, Gaggle: "goobers", Limit: 2}, // mixed
		{Stage: "implement", Outcome: OutcomeSuccess, Limit: 3},      // stage/outcome (journal-applied)
		{Since: time.Date(2026, 7, 1, 12, 5, 0, 0, time.UTC), Limit: 4},
		{LatestPerWorkflow: true},
		{Gaggle: "acme-web", LatestPerWorkflow: true},
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

func TestLatestWorkflowOutcomesIndexedMatchesScanningWithStaleStatus(t *testing.T) {
	tests := []struct {
		name string
		seed func(*testing.T, instance.Layout, *workflow.Machine) *rollup.DB
		want string
	}{
		{
			name: "finish missing from index",
			seed: func(t *testing.T, layout instance.Layout, machine *workflow.Machine) *rollup.DB {
				older, olderClock := createFixtureRun(
					t, layout, machine, "run-older", machine.Def.Name, "goobers",
					time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
					journal.Trigger{Kind: journal.TriggerManual}, false,
				)
				finishFixtureRun(t, older, olderClock, journal.PhaseCompleted)
				newer, newerClock := createFixtureRun(
					t, layout, machine, "run-newer", machine.Def.Name, "goobers",
					time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC),
					journal.Trigger{Kind: journal.TriggerManual}, false,
				)
				if err := newer.Close(); err != nil {
					t.Fatal(err)
				}
				db := buildIndex(t, layout)
				newer, _, err := journal.Recover(
					filepath.Join(layout.RunsDir(), "run-newer"),
					journal.WithClock(func() time.Time { return newerClock.now }),
				)
				if err != nil {
					t.Fatal(err)
				}
				finishFixtureRun(t, newer, newerClock, journal.PhaseCompleted)
				return db
			},
			want: "[run-newer]",
		},
		{
			name: "resume missing from index",
			seed: func(t *testing.T, layout instance.Layout, machine *workflow.Machine) *rollup.DB {
				older, olderClock := createFixtureRun(
					t, layout, machine, "run-older", machine.Def.Name, "goobers",
					time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
					journal.Trigger{Kind: journal.TriggerManual}, false,
				)
				finishFixtureRun(t, older, olderClock, journal.PhaseCompleted)
				newer, newerClock := createFixtureRun(
					t, layout, machine, "run-newer", machine.Def.Name, "goobers",
					time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC),
					journal.Trigger{Kind: journal.TriggerManual}, false,
				)
				newerClock.advance(time.Second)
				if err := newer.Append(journal.Event{
					Type:   journal.EventRunFinished,
					Status: string(journal.PhaseEscalated),
				}); err != nil {
					t.Fatal(err)
				}
				if err := newer.Close(); err != nil {
					t.Fatal(err)
				}
				db := buildIndex(t, layout)
				newer, _, err := journal.Recover(
					filepath.Join(layout.RunsDir(), "run-newer"),
					journal.WithClock(func() time.Time { return newerClock.now }),
				)
				if err != nil {
					t.Fatal(err)
				}
				newerClock.advance(time.Second)
				if err := newer.Append(journal.Event{
					Type:   journal.EventRunResumed,
					Status: string(journal.PhaseEscalated),
					Target: "implement",
				}); err != nil {
					t.Fatal(err)
				}
				if err := newer.Close(); err != nil {
					t.Fatal(err)
				}
				return db
			},
			want: "[run-older]",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, layout, machine := fixtureService(t)
			indexed, scanning := indexedAndScanning(t, layout, test.seed(t, layout, machine))
			options := RunListOptions{LatestPerWorkflow: true}
			gotIndexed := listAllPages(t, indexed, options)
			gotScanning := listAllPages(t, scanning, options)
			if fmt.Sprint(gotIndexed) != test.want {
				t.Fatalf("indexed latest workflow outcomes = %v, want %s", gotIndexed, test.want)
			}
			if fmt.Sprint(gotScanning) != fmt.Sprint(gotIndexed) {
				t.Fatalf("indexed vs scanning diverge: indexed=%v scanning=%v", gotIndexed, gotScanning)
			}
		})
	}
}

// TestListRunsIndexedPhaseFilterIsBoundedNotScanned proves the #1197
// dashboard-timeout fix: on an instance with a large terminal run history and
// only a handful of still-running runs, filtering by Phase=running must not
// open every terminal journal to find them. Before the runs.status pushdown,
// listRunsIndexed paged newest-first through the entire history opening every
// candidate's journal just to discard it via runMatches — indistinguishable
// from a full scan once the matching runs are sparse and old, which is
// exactly why the Overview page (which asks for active runs first) timed out
// on an instance with tens of thousands of accumulated runs.
func TestListRunsIndexedPhaseFilterIsBoundedNotScanned(t *testing.T) {
	_, layout, machine := fixtureService(t)
	const terminalCount = 300
	const runningCount = 3
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// The running runs are seeded oldest (they sort last in the newest-first
	// list), followed by a large block of newer terminal runs a scanning walk
	// would have to open first.
	for i := 0; i < runningCount; i++ {
		started := base.Add(time.Duration(i) * time.Minute)
		run, _ := createFixtureRun(
			t, layout, machine,
			fmt.Sprintf("run-running-%04d", i), machine.Def.Name, "goobers",
			started, journal.Trigger{Kind: journal.TriggerManual}, false,
		)
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < terminalCount; i++ {
		started := base.Add(time.Duration(runningCount+i) * time.Minute)
		run, clock := createFixtureRun(
			t, layout, machine,
			fmt.Sprintf("run-terminal-%04d", i), machine.Def.Name, "goobers",
			started, journal.Trigger{Kind: journal.TriggerManual}, false,
		)
		finishFixtureRun(t, run, clock, journal.PhaseCompleted)
	}
	indexed, _ := indexedAndScanning(t, layout, buildIndex(t, layout))

	var opened int
	openRunObserver = func(string) { opened++ }
	t.Cleanup(func() { openRunObserver = nil })

	page, err := indexed.ListRuns(context.Background(), RunListOptions{Phase: journal.PhaseRunning, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != runningCount {
		t.Fatalf("got %d running runs, want %d", len(page.Runs), runningCount)
	}
	// A scanning walk would open all terminalCount+runningCount journals to
	// reach the matches at the tail of the newest-first order. The
	// index-pushed path should open only the matching candidates.
	if opened > runningCount*2 {
		t.Fatalf("phase=running opened %d journals across a %d-run instance; expected bounded by the matching candidates, not O(total)",
			opened, terminalCount+runningCount)
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

// TestListRunsReconcileSkipsUnpublishedRunDirs proves reconcile never pays for a
// directory that holds no run.yaml. Such a directory is not a journal — the span
// exporter creates spans/ before a run publishes, and a run that never publishes
// leaves the directory behind permanently — so IngestRun can only ever fail on
// it. It used to fail expensively: WithPruneProtection took the run's journal
// lock, creating a .lock file, before discovering there was no identity to read.
// Because the failure left the directory un-indexed, every later pass retried it,
// so on an instance where these outnumber the real ingest backlog by orders of
// magnitude a pass could not finish and ListRuns never returned (#1708). The
// absence of .lock is the precise regression signal.
func TestListRunsReconcileSkipsUnpublishedRunDirs(t *testing.T) {
	_, layout, machine := fixtureService(t)
	seedVariedRuns(t, layout, machine, 6)
	indexed, _ := indexedAndScanning(t, layout, buildIndex(t, layout))

	unpublished := []string{"unpublished-a", "unpublished-b", "unpublished-c"}
	for _, name := range unpublished {
		if err := os.MkdirAll(filepath.Join(layout.RunsDir(), name, "spans"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := indexed.ListRuns(context.Background(), RunListOptions{Limit: 5}); err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	for _, name := range unpublished {
		lock := filepath.Join(layout.RunsDir(), name, ".lock")
		switch _, err := os.Stat(lock); {
		case err == nil:
			t.Errorf("reconcile took the journal lock on unpublished run dir %q; it must skip on the run.yaml stat", name)
		case !errors.Is(err, os.ErrNotExist):
			t.Fatalf("stat %s: %v", lock, err)
		}
	}
}

// TestListRunsReconcileStillIngestsPublishedRunMissingFromIndex guards the
// skip above against over-reach: a real journal absent from the index is still
// backfilled, which is reconcile's entire purpose.
func TestListRunsReconcileStillIngestsPublishedRunMissingFromIndex(t *testing.T) {
	_, layout, machine := fixtureService(t)
	seedVariedRuns(t, layout, machine, 4)
	// Index only one run, leaving the rest on disk but unknown to the index.
	db := buildIndex(t, layout, "run-0000")
	indexed, _ := indexedAndScanning(t, layout, db)

	page, err := indexed.ListRuns(context.Background(), RunListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) < 4 {
		t.Fatalf("reconcile backfilled %d runs, want every published run on disk", len(page.Runs))
	}
}
