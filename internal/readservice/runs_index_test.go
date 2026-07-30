package readservice

import (
	"context"
	"fmt"
	"path/filepath"
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

// The reconcile tests that lived here are deleted, not merely removed (#1924).
//
// They covered index completeness — that a run appearing on disk after the last
// scan is still listed — provided by reconcileIndex running on the HTTP list
// path. That mechanism is gone: it reached IngestRun -> WithPruneProtection ->
// acquireJournalLock, which is why all 40,665 run directories on the live
// instance hold a .lock file created by a READ.
//
// Every property they protected now lives in internal/readmodel/repair, off the
// request path:
//
//	discovery of an unprojected run  ->  TestSweepDiscoversAnUnprojectedRun
//	unpublished dirs are not opened  ->  TestUnpublishedIsRememberedByMtimeAndForgottenOnPromotion
//	bounded cost per pass            ->  TestSweepCostPerStepIsBoundedByBatchSize
//	progress survives restart        ->  TestSweepCursorResumesAcrossRestart
//	a read creates no lock file      ->  TestSweepNeverCreatesALockFile
//
// Plus one the old tests could not express at all, because reconcile only ever
// looked in one direction: a projected row whose journal has vanished is removed
// (TestSweepRemovesAProjectedRunWhoseJournalIsGone, which is #1943's fix).
//
// Two properties are deliberately NOT carried over. The burst-collapse and
// refresh-after-interval tests described throttling of a scan that no longer
// happens; and the mtime-skip test encoded the reasoning §6.3 shows does not
// bind, since every new run bumps its parent's mtime and the root is therefore
// always dirty on a live instance.

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

// TestIndexedListNoLongerBackfillsOnTheRequestPath records a real behaviour
// change, rather than hiding one (#1924).
//
// This test used to assert that listing through a partially-populated index
// returned every published run on disk, because reconcileIndex backfilled the
// missing ones DURING the request. That is no longer true, and the change is
// deliberate: the backfill reached IngestRun -> WithPruneProtection ->
// acquireJournalLock, so serving a list wrote a .lock file into every run
// directory it touched — 40,665 of them on the live instance, including 10,906
// that can never be ingested.
//
// The new contract: the indexed path reflects the INDEX. Completeness is
// delivered by the repair sweep, continuously and off the request path, into
// read.db — which is the path the daemon actually serves from now that the
// cutover defaults on. The trade is a bounded staleness window (one sweep cycle,
// reported as lastCycleCompletedAt) in exchange for reads that do not write.
//
// The staleness is only reachable for runs the daemon did not execute —
// imported, migrated, or externally added — because a run the daemon runs is
// recorded to intake at completion and projected within one drain interval.
func TestIndexedListNoLongerBackfillsOnTheRequestPath(t *testing.T) {
	_, layout, machine := fixtureService(t)
	seedVariedRuns(t, layout, machine, 4)
	// Index only one run, leaving the rest on disk but unknown to the index.
	db := buildIndex(t, layout, "run-0000")
	indexed, _ := indexedAndScanning(t, layout, db)

	page, err := indexed.ListRuns(context.Background(), RunListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 1 {
		t.Errorf("the indexed list returned %d runs from an index holding 1; it is still "+
			"backfilling on the request path, which is what wrote .lock files into every "+
			"run directory", len(page.Runs))
	}
}
