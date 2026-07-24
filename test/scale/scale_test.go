package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// scaleLargeEnv, when set to a float multiplier, opts a run of the harness's
// full generate+measure path into `go test` at that scale. It is off by default
// so the standard suite stays fast and CI stays green — the target-scale
// measurement is heavy and I/O-bound (see the package doc).
const scaleLargeEnv = "GOOBERS_SCALE_LARGE"

// correctnessSpec is a deliberately small instance: generation is dominated by
// the journal's per-append fsync (a full-barrier F_FULLFSYNC on macOS that
// serializes across workers), so "a few hundred runs" would push the default
// test into minutes. This many runs still exercises every read-path behavior —
// ordering, pagination, phase spread, orphan/oversized pathologies — while
// staying within a couple of seconds. Target-scale latency is proven by the
// opt-in GOOBERS_SCALE_LARGE path, not here.
func correctnessSpec(root string) Spec {
	return Spec{
		Root:            root,
		Runs:            30,
		EventsPerRun:    6,
		SpansPerRun:     1,
		SchedulerEvents: 200,
		OrphanDirs:      3,
		OversizedRuns:   2,
		Seed:            1,
	}
}

// TestGenerateProducesReadableRuns is the merge-safe correctness assertion: it
// generates a modest instance, builds the rollup, and proves the read path
// returns bounded, correct results. It asserts no latency threshold (that would
// block this PR's own merge on current code); it proves the harness produces a
// valid, readable instance the read service can serve — the foundation the
// opt-in latency guard builds on.
func TestGenerateProducesReadableRuns(t *testing.T) {
	root := t.TempDir()
	spec := correctnessSpec(root)
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gen.Runs != spec.Runs {
		t.Fatalf("generated %d runs, want %d", gen.Runs, spec.Runs)
	}
	if gen.SchedulerJournalSize == 0 {
		t.Fatal("scheduler journal is empty")
	}

	if err := rollup.Rebuild(gen.Layout.TelemetryDB(), gen.Layout.RunsDir(), gen.Layout.SchedulerDir()); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}
	db, err := rollup.Open(gen.Layout.TelemetryDB())
	if err != nil {
		t.Fatalf("open rollup: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: minimalDefinitions(),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("construct read service: %v", err)
	}
	ctx := context.Background()

	// A bounded page must be exactly the page size, newest-first, and never
	// include the injected orphan directories (which have no run.yaml).
	page, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(page.Runs) != 10 {
		t.Fatalf("first page returned %d runs, want 10 (bounded by limit)", len(page.Runs))
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor with more runs than the page size")
	}
	for _, r := range page.Runs {
		if r.ID == "" || r.Workflow == "" {
			t.Fatalf("run summary missing identity: %+v", r)
		}
	}

	// Walking every page must yield exactly the generated run count — the
	// orphan dirs are silently skipped, not surfaced or fatal.
	total := countAllRuns(t, service)
	if total != spec.Runs {
		t.Fatalf("paged through %d runs, want %d (orphan dirs must be skipped, none dropped)", total, spec.Runs)
	}

	// The full status scan (the unindexed Overview fallback) must also succeed
	// and see every run despite the pathologies.
	status, err := service.ListStatusRuns(ctx)
	if err != nil {
		t.Fatalf("ListStatusRuns: %v", err)
	}
	if len(status) != spec.Runs {
		t.Fatalf("status scan saw %d runs, want %d", len(status), spec.Runs)
	}
}

// TestOrphanDirsSurviveRollupAndScan pins the resilience contract directly: an
// orphan run directory (no run.yaml) is present on disk but must never appear in
// the rollup or the read results, and must never make either fail.
func TestOrphanDirsSurviveRollupAndScan(t *testing.T) {
	root := t.TempDir()
	spec := correctnessSpec(root)
	spec.Runs = 8
	spec.OrphanDirs = 4
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// The orphan directories really exist under runs/.
	orphans, err := filepath.Glob(filepath.Join(gen.Layout.RunsDir(), "orphan-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != spec.OrphanDirs {
		t.Fatalf("found %d orphan dirs on disk, want %d", len(orphans), spec.OrphanDirs)
	}

	if err := rollup.Rebuild(gen.Layout.TelemetryDB(), gen.Layout.RunsDir(), gen.Layout.SchedulerDir()); err != nil {
		t.Fatalf("rebuild must skip orphan dirs, not fail: %v", err)
	}
	db, err := rollup.Open(gen.Layout.TelemetryDB())
	if err != nil {
		t.Fatalf("open rollup: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: minimalDefinitions(),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("construct read service: %v", err)
	}
	if got := countAllRuns(t, service); got != spec.Runs {
		t.Fatalf("read service returned %d runs, want %d (orphans must be excluded)", got, spec.Runs)
	}
}

// TestMeasureLargeScale is the opt-in target-scale measurement. It is skipped
// unless GOOBERS_SCALE_LARGE is set to a positive multiplier (e.g. 1, 10, 100),
// so default `go test` and CI stay fast and green. It reports the read-path
// latencies at scale; it deliberately asserts no hard latency threshold here so
// the guard can be tuned against real numbers rather than blocking on an
// arbitrary bound — the numbers are the deliverable (epic #1410).
func TestMeasureLargeScale(t *testing.T) {
	raw := os.Getenv(scaleLargeEnv)
	if raw == "" {
		t.Skipf("set %s=<multiplier> (e.g. 1, 10, 100) to run the target-scale measurement", scaleLargeEnv)
	}
	mult, err := strconv.ParseFloat(raw, 64)
	if err != nil || mult <= 0 {
		t.Fatalf("%s=%q must be a positive float multiplier", scaleLargeEnv, raw)
	}

	spec := scaledSpec(t.TempDir(), mult)
	t.Logf("generating %d runs + %d scheduler events at %g× the dogfood baseline", spec.Runs, spec.SchedulerEvents, mult)
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m, err := measure(gen.Layout, gen.Runs, gen.SchedulerEvents, gen.SchedulerJournalSize)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	t.Logf("runs=%d scheduler_events=%d scheduler_journal=%s telemetry_db=%s",
		m.Runs, m.SchedulerEvents, humanBytes(m.SchedulerJournalSize), humanBytes(m.TelemetryDBSize))
	t.Logf("rollup_rebuild=%s listruns_first_page=%s listruns_warm_page=%s overview_fanout=%s status_full_scan=%s",
		m.RollupRebuild, m.ListRunsFirstPage, m.ListRunsWarmPage, m.OverviewFanout, m.StatusScan)

	// A minimal sanity bound that holds regardless of scale: the indexed page
	// must be bounded — decisively cheaper than the full unindexed status scan.
	// This is the DASH-18 index invariant, not a wall-clock threshold, so it is
	// safe to assert even at 100×.
	if m.ListRunsWarmPage >= m.StatusScan {
		t.Fatalf("indexed warm page (%s) was not faster than the full status scan (%s); the index is not paying off",
			m.ListRunsWarmPage, m.StatusScan)
	}
}

// BenchmarkListRunsFirstPage times one indexed ListRuns page against a modest
// pre-generated instance. Benchmarks do not run under default `go test`; invoke
// with `go test -run=^$ -bench=ListRunsFirstPage ./test/scale`.
func BenchmarkListRunsFirstPage(b *testing.B) {
	root := b.TempDir()
	spec := correctnessSpec(root)
	gen, err := generate(spec)
	if err != nil {
		b.Fatalf("generate: %v", err)
	}
	if err := rollup.Rebuild(gen.Layout.TelemetryDB(), gen.Layout.RunsDir(), gen.Layout.SchedulerDir()); err != nil {
		b.Fatalf("rebuild rollup: %v", err)
	}
	db, err := rollup.Open(gen.Layout.TelemetryDB())
	if err != nil {
		b.Fatalf("open rollup: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: minimalDefinitions(),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		b.Fatalf("construct read service: %v", err)
	}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50}); err != nil {
			b.Fatalf("ListRuns: %v", err)
		}
	}
}

// countAllRuns walks the ListRuns cursor to the end and returns the total,
// asserting no run is duplicated across pages.
func countAllRuns(t *testing.T, service *readservice.Local) int {
	t.Helper()
	ctx := context.Background()
	seen := map[string]bool{}
	options := readservice.RunListOptions{Limit: 10}
	for {
		page, err := service.ListRuns(ctx, options)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		for _, r := range page.Runs {
			if seen[r.ID] {
				t.Fatalf("duplicate run %q across pages", r.ID)
			}
			seen[r.ID] = true
		}
		if page.NextCursor == "" {
			return len(seen)
		}
		options.Cursor = page.NextCursor
	}
}
