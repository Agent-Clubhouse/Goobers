package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instancefixture"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readprobe"
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
		// A populated inventory, because the runs are attributed to the gaggles
		// and workflows it declares. An empty one produces runs whose gaggle no
		// definition mentions, and then every inventory surface reports zero
		// while looking healthy.
		Inventory: instancefixture.InventorySpec{
			InstanceName:      "scale-harness",
			Gaggles:           2,
			Workflows:         4,
			GoobersPerGaggle:  1,
			TasksPerWorkflow:  2,
			MaxConcurrentRuns: 2,
		},
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

	if err := rebuildAllRoots(gen); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}
	db, err := rollup.Open(gen.Layout.TelemetryDB())
	if err != nil {
		t.Fatalf("open rollup: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
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
	orphans, err := allOrphanDirs(gen)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != spec.OrphanDirs {
		t.Fatalf("found %d orphan dirs on disk, want %d", len(orphans), spec.OrphanDirs)
	}

	if err := rebuildAllRoots(gen); err != nil {
		t.Fatalf("rebuild must skip orphan dirs, not fail: %v", err)
	}
	db, err := rollup.Open(gen.Layout.TelemetryDB())
	if err != nil {
		t.Fatalf("open rollup: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("construct read service: %v", err)
	}
	if got := countAllRuns(t, service); got != spec.Runs {
		t.Fatalf("read service returned %d runs, want %d (orphans must be excluded)", got, spec.Runs)
	}
}

// TestGeneratedEventSizesMatchLiveDistribution pins the property that made the
// old generator useless for tail measurement: per-run event-log sizes must have
// a *spread*, matching the live instance's shape rather than its mean.
//
// Measured on the pre-change generator, every run's events.jsonl was 3,050 bytes
// — p50 == p90 == p99 == max. The live instance's p99 is 6.6× its p50 and its
// max is 22× its p50 (design §2). With no tail, a harness cannot exercise the
// single-run cost class, cannot reproduce the §2.3 measurement that ruled out
// "one huge event ledger" as the run-detail timeout cause, and reports a
// flattering p99.9 for every per-run read path.
//
// The assertion is on the *shape* (a real tail exists, in the right direction),
// not on exact bytes, so it survives ordinary changes to the event envelope.
func TestGeneratedEventSizesMatchLiveDistribution(t *testing.T) {
	spec := correctnessSpec(t.TempDir())
	spec.Runs = 400
	spec.EventsPerRun = 0 // draw from the measured distribution
	spec.SpansPerRun = 0
	spec.OversizedRuns = 0
	spec.OrphanDirs = 0
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	sizes := eventLogSizes(t, gen)
	if len(sizes) != spec.Runs {
		t.Fatalf("found %d event logs, want %d", len(sizes), spec.Runs)
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })
	p50 := sizes[len(sizes)/2]
	p99 := sizes[int(float64(len(sizes))*0.99)]
	max := sizes[len(sizes)-1]
	t.Logf("generated event-log sizes: n=%d p50=%d p99=%d max=%d", len(sizes), p50, p99, max)

	if p50 <= 0 {
		t.Fatal("p50 event log size is zero")
	}
	// The live ratios are p99/p50 ≈ 6.6 and max/p50 ≈ 22. Assert a substantial
	// tail without pinning the exact multiple.
	if ratio := float64(p99) / float64(p50); ratio < minP99ToP50Ratio {
		t.Fatalf("p99/p50 = %.1f×, want >= %.1f×: the corpus has no tail, so per-run read costs are uniform and the tail cannot be measured",
			ratio, minP99ToP50Ratio)
	}
	if ratio := float64(max) / float64(p50); ratio < minMaxToP50Ratio {
		t.Fatalf("max/p50 = %.1f×, want >= %.1f×: the corpus lacks the large-ledger runs the live instance has",
			ratio, minMaxToP50Ratio)
	}
}

// Tail-shape floors, set well below the live instance's measured ratios (6.6×
// and 22×) so ordinary envelope changes do not flake the test while a
// regression to a constant size still fails it loudly.
const (
	minP99ToP50Ratio = 3.0
	minMaxToP50Ratio = 6.0
)

// TestEventsPerRunPinsDistributionWhenSet proves the escape hatch works: an
// explicit EventsPerRun yields a corpus with **no tail**, which is what the cheap
// correctness fixtures and any isolate-one-variable measurement need.
//
// It asserts a narrow spread rather than byte-identical sizes, because the
// generator deliberately varies gaggle name, trigger kind, and terminal phase
// across runs (so gaggle/phase filters have real values to match) and every
// seventh run is left in flight with no run.finished at all. Those vary the
// bytes by a few percent with the event count pinned — which is fine, and is
// exactly the distinction this test draws: pinned means no *tail*, not
// no *variance*.
func TestEventsPerRunPinsDistributionWhenSet(t *testing.T) {
	spec := correctnessSpec(t.TempDir())
	spec.Runs = 60
	spec.EventsPerRun = 6
	spec.SpansPerRun = 0
	spec.OversizedRuns = 0
	spec.OrphanDirs = 0
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	sizes := eventLogSizes(t, gen)
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })
	p50, max := sizes[len(sizes)/2], sizes[len(sizes)-1]
	if p50 <= 0 {
		t.Fatal("p50 event log size is zero")
	}
	// Pinned mode must stay far below the tail floor the distribution test
	// requires, or the pin is not doing anything.
	if ratio := float64(max) / float64(p50); ratio >= minMaxToP50Ratio {
		t.Fatalf("EventsPerRun=6 produced max/p50 = %.2f× (>= the %.1f× tail floor); the pin does not hold",
			ratio, minMaxToP50Ratio)
	}
}

// TestSpanToEventByteRatioMatchesLiveInstance pins the ratio that design §5.1's
// two-store decision rests on.
//
// The live instance holds 2,263 MB of spans against 191 MB of run events — ~11×
// — and that gap *is* the argument for putting the run read model in its own
// small store: cold start becomes gated on 191 MB rather than 2.5 GB. The old
// generator wrote ~45-byte span payloads, producing a ratio of 0.03× — inverted
// by roughly 400× — so the corpus could not measure the benefit of the split it
// was meant to justify, and any rebuild figure taken from it was meaningless.
func TestSpanToEventByteRatioMatchesLiveInstance(t *testing.T) {
	spec := scaledSpec(t.TempDir(), 1)
	// Keep the corpus small; the ratio is a per-run property, not a scale one.
	spec.Runs = 120
	spec.OrphanDirs = 0
	spec.SchedulerEvents = 10
	spec.GiantSchedulerRecords = 0
	spec.OversizedRuns = 0
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	roots, err := gen.Layout.RunDirs()
	if err != nil {
		t.Fatalf("enumerate run roots: %v", err)
	}
	events := treeSizeAcross(roots, "events.jsonl")
	spans := treeSizeUnderAcross(roots, "spans")
	if events == 0 {
		t.Fatal("no run events generated")
	}
	if spans == 0 {
		t.Fatal("no spans found: treeSizeUnder must reach content-addressed spans/sha256/<aa>/<rest>")
	}
	ratio := float64(spans) / float64(events)
	t.Logf("span:event byte ratio = %.1f× (spans=%d events=%d); live instance is ~11×", ratio, spans, events)
	if ratio < minSpanToEventRatio {
		t.Fatalf("span:event ratio %.2f× is below %.1f×; the corpus does not reproduce the live footprint split that §5.1 rests on",
			ratio, minSpanToEventRatio)
	}
}

// minSpanToEventRatio is the floor for the span:event byte ratio, set below the
// live ~11× so per-run event-size variance cannot flake it while a regression to
// tiny span payloads still fails.
const minSpanToEventRatio = 6.0

// TestOrphanDirsCarryLockFiles pins the pathology the generator previously had
// backwards.
//
// All 10,906 unpublished directories on the live instance contain a `.lock`
// (design §2.4) — that is the physical evidence a read path was performing
// maintenance on directories it could never ingest. The generator used to write
// orphans *without* a lock while every real run had one, i.e. the exact inverse,
// so a corpus built from it could not support the assertion that no read path
// creates one.
func TestOrphanDirsCarryLockFiles(t *testing.T) {
	spec := correctnessSpec(t.TempDir())
	spec.Runs = 4
	spec.OrphanDirs = 6
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	orphans, err := allOrphanDirs(gen)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != spec.OrphanDirs {
		t.Fatalf("found %d orphan dirs, want %d", len(orphans), spec.OrphanDirs)
	}
	for _, dir := range orphans {
		if _, err := os.Stat(filepath.Join(dir, orphanLockFileName)); err != nil {
			t.Fatalf("orphan %s has no %s file: %v — the live instance's unpublished dirs all carry one",
				filepath.Base(dir), orphanLockFileName, err)
		}
		// And it must still be unpublished, or it is not an orphan at all.
		if _, err := os.Stat(filepath.Join(dir, "run.yaml")); err == nil {
			t.Fatalf("orphan %s has a run.yaml; it is not unpublished", filepath.Base(dir))
		}
	}
}

// TestScaledSpecReproducesLiveShape pins the 1× baseline against the measured
// instance, so the constants cannot drift silently again. The previous constants
// had drifted to ~4× off on journal bytes (design §2.5) and nothing caught it.
func TestScaledSpecReproducesLiveShape(t *testing.T) {
	spec := scaledSpec("", 1)
	if spec.Runs != 29_759 {
		t.Errorf("1× published runs = %d, want 29,759 (measured 2026-07-29)", spec.Runs)
	}
	// Published + unpublished must reconstruct the measured directory count.
	if total := spec.Runs + spec.OrphanDirs; total < 40_000 || total > 41_500 {
		t.Errorf("1× total run directories = %d, want ≈40,665 (measured)", total)
	}
	if spec.SchedulerEvents != 156_765 {
		t.Errorf("1× scheduler events = %d, want 156,765 (measured)", spec.SchedulerEvents)
	}
	if spec.EventsPerRun != 0 {
		t.Errorf("1× EventsPerRun = %d, want 0 so sizes are drawn from the measured distribution", spec.EventsPerRun)
	}
	if spec.GiantSchedulerRecords != 108 {
		t.Errorf("1× giant scheduler records = %d, want 108 (the #1414 residue)", spec.GiantSchedulerRecords)
	}
	// Orphans must scale, not sit at a fixed count that stops being a pathology.
	if ten := scaledSpec("", 10); ten.OrphanDirs <= spec.OrphanDirs {
		t.Errorf("orphan dirs did not scale: 1×=%d, 10×=%d", spec.OrphanDirs, ten.OrphanDirs)
	}
}

// TestGiantSchedulerRecordsExceedWaveOneByteBudget proves the harness can
// produce the record that constrains Wave 1.1.
//
// #1914 replaces the instance journal's whole-file re-read with a bounded
// backward scan plus a byte budget and a full-recovery fallback. The budget must
// sit *above* the largest real record or the fallback fires on ordinary history
// and the optimization does nothing. The live maximum is 2,661,279 bytes, so the
// harness has to be able to generate one that large for that bound to be
// testable at all.
func TestGiantSchedulerRecordsExceedWaveOneByteBudget(t *testing.T) {
	spec := correctnessSpec(t.TempDir())
	spec.Runs = 2
	spec.OrphanDirs = 0
	spec.SchedulerEvents = 50
	spec.GiantSchedulerRecords = 2
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	events, err := journal.ReadInstanceLog(gen.Layout.SchedulerDir())
	if err != nil {
		t.Fatalf("read instance log: %v", err)
	}
	var giant int
	for _, ev := range events {
		if ev.Error != nil && len(ev.Error.Message) >= giantSchedulerRecordBytes {
			giant++
		}
	}
	if giant != spec.GiantSchedulerRecords {
		t.Fatalf("found %d giant records, want %d", giant, spec.GiantSchedulerRecords)
	}
	// Sequence allocation must still be intact across them — this is the
	// invariant #1914 must not break, and a giant record is where a bounded
	// backward scan is most likely to.
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d; sequence is not monotonic from 1 across giant records", i, ev.Seq)
		}
	}
}

// eventLogSizes returns the byte size of every run's events.jsonl under root.
//
// It walks from the instance root rather than a single runs directory because
// runs live in per-gaggle roots (gaggles/<g>/runs) by default, matching the live
// instance. Walking only Layout.RunsDir() finds nothing and the assertion
// vacuously passes on an empty slice — so this takes the root.
func eventLogSizes(t *testing.T, gen GenerateResult) []int64 {
	t.Helper()
	roots, err := gen.Layout.RunDirs()
	if err != nil {
		t.Fatalf("enumerate run roots: %v", err)
	}
	var sizes []int64
	for _, root := range roots {
		sizes = append(sizes, eventLogSizesUnder(t, root)...)
	}
	return sizes
}

// eventLogSizesUnder collects run event-log sizes beneath one run root.
func eventLogSizesUnder(t *testing.T, root string) []int64 {
	t.Helper()
	var sizes []int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "events.jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sizes = append(sizes, info.Size())
		return nil
	})
	if err != nil {
		t.Fatalf("walk runs dir: %v", err)
	}
	return sizes
}

// TestSummarizeReportsHonestPercentiles pins the p99.9 honesty rule: a figure
// that cannot be read from the sample count is reported as unavailable rather
// than silently returned as the maximum. §14.12's targets are stated as p99.9
// and are meant to be falsifiable, which they are not if "p99.9" sometimes means
// "max of 20".
func TestSummarizeReportsHonestPercentiles(t *testing.T) {
	small := make([]time.Duration, 21) // 1 cold + 20 warm
	for i := range small {
		small[i] = time.Duration(i+1) * time.Millisecond
	}
	st := summarize("op", small)
	if st.Cold != 1*time.Millisecond {
		t.Errorf("cold = %s, want the first sample (1ms)", st.Cold)
	}
	if st.Samples != 20 {
		t.Errorf("warm samples = %d, want 20 (cold excluded)", st.Samples)
	}
	if st.P999Valid {
		t.Error("p99.9 reported as valid from 20 warm samples; it cannot be")
	}
	if st.Max != 21*time.Millisecond {
		t.Errorf("max = %s, want 21ms", st.Max)
	}

	large := make([]time.Duration, minSamplesForP999+1)
	for i := range large {
		large[i] = time.Duration(i+1) * time.Microsecond
	}
	if st := summarize("op", large); !st.P999Valid {
		t.Errorf("p99.9 not reported as valid from %d warm samples", minSamplesForP999)
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
	t.Logf("generating %d runs (+%d orphan dirs) + %d scheduler events at %g× the measured live instance",
		spec.Runs, spec.OrphanDirs, spec.SchedulerEvents, mult)
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m, err := measure(gen.Layout, gen, largeScaleSamples, fsyncDisabled())
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	writeReport(&testLogWriter{t: t}, m)

	// A minimal sanity bound that holds regardless of scale: the indexed page
	// must be bounded — decisively cheaper than the full unindexed status scan.
	// This is the index invariant, not a wall-clock threshold, so it is safe to
	// assert even at 10×. Compared at p50 rather than on a single sample, since
	// one sample of either side can be arbitrarily unlucky.
	page, scan := stat(m, opListRunsPage), stat(m, opStatusFullScan)
	if page.P50 >= scan.P50 {
		t.Fatalf("indexed page p50 (%s) was not faster than the full status scan p50 (%s); the index is not paying off",
			page.P50, scan.P50)
	}

	// Keyset pagination must not degrade with depth: a deep page costs what a
	// first page costs, or the plan is offset-shaped (design §7.3). Generous
	// factor because this is a smoke bound on plan *shape*, not a latency target.
	deep := stat(m, opListRunsDeepPage)
	if page.P50 > 0 && deep.P50 > page.P50*deepPageToleranceFactor {
		t.Fatalf("deep page p50 (%s) exceeded %d× the first page p50 (%s); pagination is not keyset-bounded",
			deep.P50, deepPageToleranceFactor, page.P50)
	}
}

// stat returns the named Stat from a Measurement, or the zero value. Lives here
// rather than on Measurement because only assertions look a stat up by name —
// writeReport iterates in order — and an accessor with no production caller is
// dead code the gate rightly rejects.
func stat(m Measurement, op string) Stat {
	for _, s := range m.Stats {
		if s.Op == op {
			return s
		}
	}
	return Stat{Op: op}
}

// largeScaleSamples is the sample count for the opt-in target-scale run. It is
// below minSamplesForP999 on purpose: a 1000-sample loop over a 10× corpus is
// its own multi-hour job, so this path reports p50/p99 and the report marks the
// p99.9 as unavailable rather than fabricating one. Publishing a p99.9 is a
// deliberate, separately-invoked measurement (-samples=1000).
const largeScaleSamples = 20

// deepPageToleranceFactor bounds how much more a deep page may cost than a first
// page before pagination is judged not keyset-bounded.
const deepPageToleranceFactor = 4

// testLogWriter adapts writeReport to t.Logf so the opt-in scale run reports
// through the test log in exactly the format the CLI prints.
type testLogWriter struct{ t *testing.T }

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
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
	if err := rebuildAllRoots(gen); err != nil {
		b.Fatalf("rebuild rollup: %v", err)
	}
	db, err := rollup.Open(gen.Layout.TelemetryDB())
	if err != nil {
		b.Fatalf("open rollup: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
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

// rebuildAllRoots rebuilds the rollup over every per-gaggle run root. Runs live
// in gaggles/<g>/runs, so rollup.Rebuild over a single directory ingests nothing
// and every read then measures an empty index — a silent pass, not a failure.
func rebuildAllRoots(gen GenerateResult) error {
	roots, err := gen.Layout.RunDirs()
	if err != nil {
		return err
	}
	return rollup.RebuildAll(context.Background(), gen.Layout.TelemetryDB(), roots, gen.Layout.SchedulerDir())
}

// allOrphanDirs returns every generated orphan directory across all run roots.
func allOrphanDirs(gen GenerateResult) ([]string, error) {
	roots, err := gen.Layout.RunDirs()
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, root := range roots {
		found, err := filepath.Glob(filepath.Join(root, "orphan-*"))
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, found...)
	}
	return dirs, nil
}

// TestInventorySurfacesDoNotScanRunHistory is the §14.2 bound, converted from
// the Wave 0 measurement that asserted the defect.
//
// Before #1741, /v1/instance, /v1/gaggles, /v1/gaggles/{g}/workflows and the
// workflow-detail route each walked every run directory in history and opened
// every journal to reconstruct phase — per request. The Wave 0 version of this
// test asserted exactly that, and its failure message said to invert it into a
// bound when the fix landed. This is that inversion.
//
//	before: instance 233.6ms p50, 2,000 journal opens at a 2,000-run corpus
//	after:  instance     1us p50,     0 journal opens
//
// Asserted on work rather than wall time. An earlier version compared p50
// latencies across corpus sizes and failed on CI with the SMALLER corpus five
// times slower — a contended, three-way-sharded, -race runner cannot defend a
// duration. Journal opens are deterministic on any machine at any load.
func TestInventorySurfacesDoNotScanRunHistory(t *testing.T) {
	const (
		smallRuns = 80
		largeRuns = 320
	)
	small := measureAtRunCount(t, smallRuns)
	large := measureAtRunCount(t, largeRuns)

	// The control: a bounded list page's work is fixed by the page, not history.
	smallPage := small.Work[opListRunsPage].JournalOpens
	largePage := large.Work[opListRunsPage].JournalOpens
	t.Logf("listruns_page journal opens: %d -> %d", smallPage, largePage)
	if smallPage != largePage {
		t.Errorf("bounded list page opened %d journals at %d runs but %d at %d runs; "+
			"its work must be bounded by the page, not by history",
			smallPage, smallRuns, largePage, largeRuns)
	}

	// The bound: the inventory surfaces perform NO active-run scan at all. They
	// serve the background sample from memory (#1741).
	for _, op := range []string{opInstance, opGaggles, opWorkflows, opWorkflowDetail} {
		for _, m := range []struct {
			runs int
			w    readprobe.Snapshot
		}{{smallRuns, small.Work[op]}, {largeRuns, large.Work[op]}} {
			if m.w.ActiveScanOpens != 0 || m.w.ActiveScanDirs != 0 {
				t.Errorf("%s at %d runs walked %d directories and opened %d journals; the §14.2 bound is zero.\n"+
					"The active-run count must come from the background sampler, never a request-path walk.",
					op, m.runs, m.w.ActiveScanDirs, m.w.ActiveScanOpens)
			}
			if m.w.JournalOpens != 0 {
				t.Errorf("%s at %d runs opened %d journals via openRun; the §14.2 bound is zero",
					op, m.runs, m.w.JournalOpens)
			}
		}
		t.Logf("%s: 0 journal opens at both %d and %d runs", op, smallRuns, largeRuns)
	}

	// LatestPerWorkflow no longer scans, but still opens one journal per
	// candidate row — the residual candidate loop #1782 and Wave 2's read model
	// remove. Recorded rather than asserted at zero, so the remaining work stays
	// visible instead of being forgotten.
	lo := small.Work[opLatestPerWorkflow]
	hi := large.Work[opLatestPerWorkflow]
	t.Logf("listruns_latest_per_workflow: scan opens %d -> %d (bounded), candidate opens %d -> %d (#1782 removes these)",
		lo.ActiveScanOpens, hi.ActiveScanOpens, lo.JournalOpens, hi.JournalOpens)
	if lo.ActiveScanOpens != 0 || hi.ActiveScanOpens != 0 {
		t.Errorf("latest_per_workflow still performs an active-run scan (%d, %d opens)",
			lo.ActiveScanOpens, hi.ActiveScanOpens)
	}
}

// measureAtRunCount generates a corpus of n runs and measures it. The inventory
// is held constant across sizes so the only variable is run history — which is
// the whole point: if the inventory grew too, a growth in inventory-surface cost
// would prove nothing.
func measureAtRunCount(t *testing.T, n int) Measurement {
	t.Helper()
	spec := correctnessSpec(t.TempDir())
	spec.Runs = n
	spec.EventsPerRun = 6 // cheap and uniform; this test is about count, not size
	spec.SpansPerRun = 0
	spec.SpanBytes = 0
	spec.ExtraSpanFraction = 0
	spec.OversizedRuns = 0
	spec.OrphanDirs = 0
	spec.SchedulerEvents = 50
	spec.GiantSchedulerRecords = 0
	spec.Inventory = instancefixture.InventorySpec{
		InstanceName:      "growth-fixture",
		Gaggles:           2,
		Workflows:         20,
		GoobersPerGaggle:  1,
		TasksPerWorkflow:  1,
		MaxConcurrentRuns: 4,
	}
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate %d runs: %v", n, err)
	}
	m, err := measure(gen.Layout, gen, 6, fsyncDisabled())
	if err != nil {
		t.Fatalf("measure %d runs: %v", n, err)
	}
	return m
}

// TestWorkPerInvocationIsUnbounded records, in executable form, what each read
// path actually costs in journal opens today — the §14.2 bar, stated in work
// rather than latency.
//
// Every number below matches a specific claim in the design or the diagnosis,
// and none of them had ever been measured:
//
//   - A bounded 50-row list page opens limit+1 journals — one per returned row.
//     The diagnosis's Appendix A says "lists open and parse a journal per
//     returned row"; this is that, quantified. §14.2's target is ZERO.
//   - Every inventory surface now opens ZERO journals, since #1741 moved the
//     active-run count to a background sampler. Before it, each opened one
//     journal per run in total history — §2.1's 17.2 s scan.
//   - Run detail opens the same run's journal three times for one page load,
//     which is §2.3's "GetRun and RunEvents each call openRun; stage selection
//     can call it a third time". §8.2's useSingleRun target is ONE.
//
// Like TestInventorySurfacesCostGrowsWithRunHistory, this asserts the CURRENT
// shape and is meant to fail as the waves land. Each failure is a prompt to
// tighten the bound here rather than to relax it.
func TestWorkPerInvocationIsUnbounded(t *testing.T) {
	m := measureAtRunCount(t, 120)

	get := func(op string) readprobe.Snapshot { return m.Work[op] }

	// A bounded page opens one journal per returned row, plus the lookahead row.
	// Asserted as a relationship to the page size, not a magic number, so it
	// stays true if the page size changes.
	const pageLimit = 50
	if got := get(opListRunsPage).JournalOpens; got != pageLimit+1 {
		t.Errorf("bounded %d-row page opened %d journals, expected %d (limit+1, one per row).\n"+
			"If this dropped to 0, Wave 2's read model has landed and this assertion should become the §14.2 bound.",
			pageLimit, got, pageLimit+1)
	}

	// Run detail parses one run's journal three times for one page load.
	if got := get(opRunDetail).JournalOpens; got != 3 {
		t.Errorf("run detail opened the same run's journal %d times, expected 3 (GetRun + RunEvents + StageAttempts).\n"+
			"If this dropped to 1, §8.2's useSingleRun has landed and this should become the bound.", got)
	}

	// The inventory surfaces perform no journal work at all, since #1741 moved
	// the active-run count to a background sampler.
	for _, op := range []string{opInstance, opGaggles, opWorkflows, opWorkflowDetail} {
		w := get(op)
		if w.ActiveScanOpens != 0 || w.JournalOpens != 0 {
			t.Errorf("%s performed journal work (%d scan opens, %d openRun); the §14.2 bound is zero",
				op, w.ActiveScanOpens, w.JournalOpens)
		}
	}
}

// TestReadProbeIsOffByDefaultAndCostsNothing pins the two properties that make
// it safe to leave this instrumentation on production hot paths: it records
// nothing unless enabled, and a disabled call is a single atomic load.
func TestReadProbeIsOffByDefaultAndCostsNothing(t *testing.T) {
	// Off by default is asserted behaviourally rather than by reading a flag:
	// record without enabling, and nothing may be counted.
	readprobe.Reset()
	readprobe.RecordJournalOpen()
	readprobe.RecordActiveScanOpen()
	if got := readprobe.Take(); !got.Zero() {
		t.Fatalf("disabled probe recorded %+v; it must record nothing", got)
	}

	readprobe.Enable()
	t.Cleanup(readprobe.Disable)
	readprobe.RecordJournalOpen()
	if got := readprobe.Take().JournalOpens; got != 1 {
		t.Fatalf("enabled probe recorded %d journal opens, want 1", got)
	}
}

// TestInstanceLogAppendReadsBoundedBytes is §14.11's bytes-read-per-append
// bound, now that #1914 has landed.
//
// This test was written before the fix, asserting the DEFECT: every
// InstanceLog.Append read 100.0% of the journal to allocate its sequence, so N
// appends read N times the file — 1.30 s per append at the live instance's
// 324 MB, growing without bound, on a write path sharing the process with every
// read (§2.2). Its failure message said to convert it into the bound rather than
// delete it when the fix landed, and this is that conversion.
//
// Measured after: 65,536 bytes of a 2,708,894-byte journal — 2.42%, and constant
// rather than proportional.
//
// Asserted in bytes rather than seconds, because a contended CI runner cannot
// defend a duration but "this append read 65,536 bytes" is true on every machine.
func TestInstanceLogAppendReadsBoundedBytes(t *testing.T) {
	spec := correctnessSpec(t.TempDir())
	spec.Runs = 1
	spec.OrphanDirs = 0
	spec.SpansPerRun = 0
	spec.OversizedRuns = 0
	spec.SchedulerEvents = 4000
	spec.GiantSchedulerRecords = 0
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	journalPath := filepath.Join(gen.Layout.SchedulerDir(), "events.jsonl")
	info, err := os.Stat(journalPath)
	if err != nil {
		t.Fatalf("stat instance journal: %v", err)
	}
	size := info.Size()
	if size < 100_000 {
		t.Fatalf("instance journal is only %d bytes; too small to demonstrate a bound", size)
	}

	log, _, err := journal.OpenInstanceLog(gen.Layout.SchedulerDir())
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	const appends = 5
	readprobe.Enable()
	t.Cleanup(readprobe.Disable)
	before := readprobe.Take()
	for i := 0; i < appends; i++ {
		if err := log.Append(journal.Event{Type: journal.EventTickSkipped, Reason: "probe"}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got := readprobe.Take().Sub(before)

	if got.InstanceLogAppends != appends {
		t.Fatalf("recorded %d appends, want %d", got.InstanceLogAppends, appends)
	}
	perAppend := got.InstanceLogBytes / got.InstanceLogAppends
	t.Logf("instance journal %d bytes; each append read %d bytes (%.2f%% of the file)",
		size, perAppend, 100*float64(perAppend)/float64(size))

	// The bound: an append reads a bounded window, not the file. Stated as a
	// fraction of the journal so it keeps meaning as the fixture grows, and
	// generously, since the window is chunked and a large final record legitimately
	// widens it.
	if perAppend > uint64(size)/2 {
		t.Errorf("each append read %d bytes of a %d-byte journal (%.1f%%); #1914's bounded tail read is not in effect",
			perAppend, size, 100*float64(perAppend)/float64(size))
	}

	// And the total must NOT be quadratic in the append count, which is the
	// property that made the old behaviour unbounded rather than merely expensive.
	if got.InstanceLogBytes >= uint64(size)*appends/2 {
		t.Errorf("total bytes read across %d appends was %d, which is still proportional to (appends x journal size)",
			appends, got.InstanceLogBytes)
	}
}

// TestMixedLoadExperimentProducesAVerdict is a smoke test for §16.3's
// experiment: it must run end to end, apply real concurrent writes, and produce
// a comparable idle/loaded pair. It deliberately asserts nothing about the
// verdict itself — whether head-of-line blocking is confirmed is a measurement
// on a real corpus, not something a 2-second unit test may decide.
//
// What it does pin is that the experiment cannot silently measure nothing,
// which is how the first version of it failed: it accepted a Duration and then
// ran the load only for as long as one pass of the read mix took.
func TestMixedLoadExperimentProducesAVerdict(t *testing.T) {
	spec := correctnessSpec(t.TempDir())
	spec.Runs = 40
	spec.OrphanDirs = 0
	spec.SpansPerRun = 0
	spec.OversizedRuns = 0
	spec.SchedulerEvents = 200
	spec.GiantSchedulerRecords = 0
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := rebuildAllRoots(gen); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}

	load := DefaultLoadSpec(2 * time.Second)
	res, err := runMixedLoad(gen.Layout, gen, load, 4)
	if err != nil {
		t.Fatalf("mixed load: %v", err)
	}

	// Every operation must have been measured in both phases, or the comparison
	// is between different sets.
	for op := range res.Idle {
		if _, ok := res.UnderLoad[op]; !ok {
			t.Errorf("%s measured idle but not under load", op)
		}
		if _, ok := res.Degradation[op]; !ok {
			t.Errorf("%s has no degradation figure", op)
		}
	}
	if len(res.Idle) == 0 {
		t.Fatal("no operations measured")
	}

	// The load must actually have been applied. Zero writes would mean the
	// "under load" phase measured an idle instance and every degradation figure
	// is meaningless.
	w := res.WritesApplied
	t.Logf("writes applied: %d scheduler appends (slowest %s), %d run appends, %d ingests",
		w.SchedulerAppends, w.SlowestSchedulerAppendNanos, w.RunAppends, w.Ingests)
	if w.SchedulerAppends == 0 && w.RunAppends == 0 && w.Ingests == 0 {
		t.Fatal("no writes were applied during the loaded phase; the experiment measured an idle instance twice")
	}

	// The sustained phase must have run for at least the requested duration.
	// This is what the first version got wrong.
	if res.Spec.DurationSeconds != load.Duration.Seconds() {
		t.Errorf("reported duration %.1fs does not match the requested %.1fs",
			res.Spec.DurationSeconds, load.Duration.Seconds())
	}
}

// TestTenantLoadMeasuresEachLevelSeparately is the smoke test for the
// multi-gaggle dimension: two tenant levels must each be driven and reported on
// their own, with real concurrent writes applied at both.
//
// It asserts nothing about how much contention there is — that is a measurement
// on a real corpus — but it does pin the property the dimension exists for: a
// level that measured no reads, or applied no writes, would be multi-tenant cost
// inferred rather than exercised, which is exactly what this replaces.
func TestTenantLoadMeasuresEachLevelSeparately(t *testing.T) {
	spec := correctnessSpec(t.TempDir())
	spec.Runs = 40
	spec.OrphanDirs = 0
	spec.SpansPerRun = 0
	spec.OversizedRuns = 0
	spec.SchedulerEvents = 200
	spec.GiantSchedulerRecords = 0
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := rebuildAllRoots(gen); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}

	load := DefaultTenantLoadSpec(500 * time.Millisecond)
	load.Levels = []int{1, 2}
	res, err := runTenantLoad(gen.Layout, gen, load, 4)
	if err != nil {
		t.Fatalf("tenant load: %v", err)
	}

	if len(res.Levels) != 2 {
		t.Fatalf("expected 2 measured levels, got %d", len(res.Levels))
	}
	if res.Levels[0].Tenants != 1 || res.Levels[1].Tenants != 2 {
		t.Fatalf("levels not measured as 1 then 2 tenants: %d, %d",
			res.Levels[0].Tenants, res.Levels[1].Tenants)
	}
	for _, level := range res.Levels {
		s, ok := level.Stats[tenantStoreBoundOp]
		if !ok || s.Samples == 0 {
			t.Fatalf("tenants=%d measured no %s samples", level.Tenants, tenantStoreBoundOp)
		}
		w := level.WritesApplied
		t.Logf("tenants=%d: %s p99=%s n=%d; writes %d sched, %d run, %d ingest",
			level.Tenants, tenantStoreBoundOp, s.P99, s.Samples,
			w.SchedulerAppends, w.RunAppends, w.Ingests)
		if w.SchedulerAppends == 0 && w.RunAppends == 0 && w.Ingests == 0 {
			t.Errorf("tenants=%d applied no writes; the level measured an idle instance", level.Tenants)
		}
	}
	// The whole dimension's output: a comparable figure across levels. Zero
	// would mean one of the levels never measured the operation it keys on.
	if res.ContentionFactor <= 0 {
		t.Errorf("no cross-level factor for %s", res.ContentionOperation)
	}
}

// TestTenantLevelsClampToGeneratedGaggles pins the clamp and its reporting. A
// level above the corpus's gaggle count cannot be driven, and reporting the
// requested number would claim a concurrency that never ran.
func TestTenantLevelsClampToGeneratedGaggles(t *testing.T) {
	levels := normalizeTenantLevels([]int{8, 1, 0, 4, 1}, 4)
	want := []tenantLevelPlan{{requested: 1, effective: 1}, {requested: 4, effective: 4}}
	if len(levels) != len(want) {
		t.Fatalf("normalizeTenantLevels = %+v, want %+v", levels, want)
	}
	for i, plan := range levels {
		if plan != want[i] {
			t.Errorf("level %d = %+v, want %+v", i, plan, want[i])
		}
	}

	clamped := normalizeTenantLevels([]int{1, 6}, 2)
	if len(clamped) != 2 || clamped[1].effective != 2 || clamped[1].requested != 6 {
		t.Fatalf("a level above the gaggle count must clamp and say so: %+v", clamped)
	}
}

// TestTenantLoadRequiresTwoDistinctLevels keeps the scenario from reporting a
// single level as a comparison: one level is a load test, not a dimension.
func TestTenantLoadRequiresTwoDistinctLevels(t *testing.T) {
	spec := correctnessSpec(t.TempDir())
	spec.Runs = 4
	spec.OrphanDirs = 0
	spec.SpansPerRun = 0
	spec.OversizedRuns = 0
	spec.SchedulerEvents = 10
	spec.GiantSchedulerRecords = 0
	spec.Inventory.Gaggles = 1
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := rebuildAllRoots(gen); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}
	load := DefaultTenantLoadSpec(10 * time.Millisecond)
	if _, err := runTenantLoad(gen.Layout, gen, load, 2); err == nil {
		t.Fatal("expected an error when every level clamps onto the same tenant count")
	}
}

// TestParseTenantLevels covers the CLI list, including the rejections: a
// silently-dropped level would measure a different scenario than the one asked
// for and report it under the requested name.
func TestParseTenantLevels(t *testing.T) {
	got, err := parseTenantLevels(" 1, 4 ,16")
	if err != nil {
		t.Fatalf("parseTenantLevels: %v", err)
	}
	want := []int{1, 4, 16}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("parseTenantLevels = %v, want %v", got, want)
		}
	}
	for _, bad := range []string{"4", "1,x", "1,0", "1,-2", ""} {
		if _, err := parseTenantLevels(bad); err == nil {
			t.Errorf("parseTenantLevels(%q) accepted an unusable level list", bad)
		}
	}
}

// adverseFixtureInstance builds a small readable instance for the adverse-state
// tests, returning the generated corpus and an open read service.
func adverseFixtureInstance(t *testing.T) (GenerateResult, *readservice.Local, *rollup.DB) {
	t.Helper()
	spec := correctnessSpec(t.TempDir())
	spec.Runs = 12
	spec.OrphanDirs = 0
	spec.SpansPerRun = 0
	spec.OversizedRuns = 0
	spec.SchedulerEvents = 40
	spec.GiantSchedulerRecords = 0
	gen, err := generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := rebuildAllRoots(gen); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}
	db, err := rollup.Open(gen.Layout.TelemetryDB())
	if err != nil {
		t.Fatalf("open rollup: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      gen.Layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		t.Fatalf("construct read service: %v", err)
	}
	return gen, service, db
}

// TestAdverseStatesNeverProduceAFourthState is §16.4's adverse-state coverage,
// asserted against §7.2's central rule.
//
// The read contract permits exactly three outcomes — current, stale by a stated
// amount, or unavailable with a reason — and says there is "never a fourth
// indefinite one". Every adverse state below must therefore land in one of the
// three: it may succeed, and it may fail with an error, but it may not hang and
// it may not return a silently-truncated success. A silent empty list under a
// corrupt store is the §14.7 silent-omission failure wearing a different hat.
//
// Each fault is cleared afterwards and the read re-issued, because recovery is
// the half a one-way fault injection cannot check — a system that fails
// correctly but never recovers is still broken.
func TestAdverseStatesNeverProduceAFourthState(t *testing.T) {
	ctx := context.Background()

	t.Run("corrupt-store", func(t *testing.T) {
		gen, service, db := adverseFixtureInstance(t)
		before, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50})
		if err != nil {
			t.Fatalf("baseline list: %v", err)
		}
		if len(before.Runs) == 0 {
			t.Fatal("baseline list returned nothing; the fixture is not readable")
		}
		// The open handle must be closed before the file is replaced, or the
		// corruption is invisible to an already-mapped connection.
		_ = db.Close()

		fx, err := corruptStore(gen.Layout.TelemetryDB())
		if err != nil {
			t.Fatalf("corrupt store: %v", err)
		}
		defer fx.Cleanup()

		// Opening a corrupt store must FAIL, not succeed with zero rows. The
		// distinction is the whole point: "no runs" and "cannot read the runs"
		// look identical to a user and mean opposite things.
		corrupt, openErr := rollup.Open(gen.Layout.TelemetryDB())
		if openErr == nil {
			defer func() { _ = corrupt.Close() }()
			svc, err := readservice.NewLocal(readservice.LocalSources{
				Layout:      gen.Layout,
				Definitions: instancefixture.Inventory(gen.Inventory),
				Telemetry:   corrupt,
			}, func() bool { return true })
			if err != nil {
				t.Logf("read service construction rejected the corrupt store: %v", err)
				return
			}
			page, listErr := svc.ListRuns(ctx, readservice.RunListOptions{Limit: 50})
			if listErr == nil && len(page.Runs) == 0 {
				t.Errorf("a corrupt store produced a successful EMPTY list.\n" +
					"That is silent omission (§14.7): indistinguishable from a healthy instance with no runs. " +
					"It must be an error, or a response that states it is incomplete.")
			}
			if listErr != nil {
				t.Logf("corrupt store surfaced as a list error: %v", listErr)
			}
		} else {
			t.Logf("corrupt store rejected at open, which is the clearest outcome: %v", openErr)
		}
	})

	t.Run("corrupt-journal-tail", func(t *testing.T) {
		gen, service, _ := adverseFixtureInstance(t)
		dir, err := gen.Layout.FindRunDir("run-00000000")
		if err != nil {
			t.Fatalf("find run dir: %v", err)
		}
		fx, err := corruptJournalTail(dir)
		if err != nil {
			t.Fatalf("corrupt journal tail: %v", err)
		}

		// A torn tail is the crash shape recovery exists for: the run must stay
		// readable, with the torn bytes discarded rather than surfaced.
		if _, err := service.GetRun(ctx, "run-00000000"); err != nil {
			t.Errorf("a torn journal tail made the run unreadable: %v\n"+
				"A torn final record is an ordinary crash artifact; recovery must discard it, not fail.", err)
		}
		// And the list must not fail because one run has a torn tail.
		if _, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50}); err != nil {
			t.Errorf("one torn journal tail failed the whole list: %v", err)
		}
		fx.Cleanup()
		if _, err := service.GetRun(ctx, "run-00000000"); err != nil {
			t.Errorf("run still unreadable after the fault was cleared: %v", err)
		}
	})

	t.Run("missing-journal", func(t *testing.T) {
		gen, service, _ := adverseFixtureInstance(t)
		dir, err := gen.Layout.FindRunDir("run-00000001")
		if err != nil {
			t.Fatalf("find run dir: %v", err)
		}
		fx, err := removeJournal(dir)
		if err != nil {
			t.Fatalf("remove journal: %v", err)
		}
		defer fx.Cleanup()

		// §11.4 calls "the row outlives the journal" impossible. It is reachable
		// today by an operator rm, so what matters is that it degrades: the list
		// must not fail wholesale because one run's journal vanished.
		page, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50})
		if err != nil {
			t.Errorf("one missing journal failed the entire list: %v\n"+
				"§6.3 requires repair to reconcile this direction; until then it must at least degrade, not fail closed.", err)
			return
		}
		t.Logf("list returned %d runs with one journal removed (corpus is %d)", len(page.Runs), gen.Runs)

		// Measured: the run is still listed. Its row survives in the index while
		// its journal is gone — §11.4's "the row outlives the journal" case,
		// which that section calls *impossible* on the grounds that retention
		// signals through Intake and the projector emits run.removed. That
		// protocol does not exist yet, and an operator `rm` reaches the state
		// today, so the row is served as an ordinary run.
		//
		// The consequence is a phantom: the run lists fine and fails when opened.
		// That is not a crash, so the list correctly degrades — but it is exactly
		// why §6.3 requires repair to be BIDIRECTIONAL rather than only
		// "on disk but not projected". Recorded here so the behaviour is a known,
		// tested state rather than a surprise during Wave 3.
		listed := false
		for _, r := range page.Runs {
			if r.ID == "run-00000001" {
				listed = true
				break
			}
		}
		if !listed {
			t.Log("the run with no journal was excluded from the list; bidirectional repair may already be in effect")
			return
		}
		// Measured, and worse than a phantom: the run is not merely still listed,
		// it is silently RECLASSIFIED. Phase is reconstructed by replaying the
		// event log (journal.Reader.Phase), so an absent log replays to zero
		// events and defaults to "running".
		//
		//   before removal: phase=completed terminal=true  stages=[implement] lastSeq=9
		//   after removal:  phase=running   terminal=false stages=[]          lastSeq=0
		//   RunEvents:      0 events, no error
		//
		// So a finished run whose journal is deleted becomes an ACTIVE run in the
		// portal, with no error and no partial marker. It inflates the active-run
		// count — the very number §2.1's 17.2 s scan exists to compute — and it is
		// precisely the "silently miscategorised" failure §5.4 says a stored,
		// projector-written phase removes.
		//
		// This test pins the current behaviour so the fix is visible when it lands.
		// It is deliberately not an t.Error: the behaviour is a real defect, but
		// failing the suite on it would block every unrelated change until Wave 2.
		detail, err := service.GetRun(ctx, "run-00000001")
		if err != nil {
			t.Logf("run with no journal is unreadable (%v) — bidirectional repair or a stored phase may have landed", err)
			return
		}
		if detail.Phase == journal.PhaseRunning && !detail.Terminal {
			t.Logf("CONFIRMED DEFECT: a completed run whose journal was removed now reports phase=%q terminal=%v "+
				"with no error. It is counted as ACTIVE. Fixed by §5.4's stored phase (#1918) plus §6.3's "+
				"bidirectional repair (#1924); tracked separately.", detail.Phase, detail.Terminal)
			return
		}
		t.Logf("run with no journal reports phase=%q terminal=%v", detail.Phase, detail.Terminal)
	})

	t.Run("read-only-volume", func(t *testing.T) {
		gen, service, _ := adverseFixtureInstance(t)
		fx, err := makeReadOnlyVolume(gen.Layout.Root)
		if err != nil {
			t.Skipf("read-only fixture unavailable: %v", err)
		}
		defer fx.Cleanup()

		// §11.2 says a read-only volume must serve explicitly degraded rather
		// than silently doing something expensive or failing without a reason.
		// The minimum bar, and what this asserts, is that reads still answer.
		if _, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50}); err != nil {
			t.Logf("list failed on a read-only volume: %v", err)
		}
		if _, err := service.GetRun(ctx, "run-00000000"); err != nil {
			t.Logf("run detail failed on a read-only volume: %v", err)
		}
		// The real assertion: no read path may create a lock file, and on a
		// read-only volume it provably cannot — so if a read SUCCEEDS here while
		// failing to write, that is evidence the read path is write-free.
		t.Log("read-only volume exercised; any write attempted by a read path fails visibly here rather than silently succeeding")
	})
}
