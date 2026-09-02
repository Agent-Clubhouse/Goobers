package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/instancefixture"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// The mixed-load experiment.
//
// Design §2.3 names head-of-line blocking as the *leading measured hypothesis*
// for run-detail timeouts — explicitly a hypothesis, not a conclusion — and §16.3
// makes this experiment its arbiter. §17's Wave 0 exit gates the claim on it.
//
// The hypothesis: a single SQLite connection (SetMaxOpenConns(1)) shared by every
// reader and writer, plus 1.3 s locked instance-journal appends, plus 4–17 s
// active-run scans, means a read's latency is determined by what *else* is
// happening rather than by its own cost. If true, the same read is fast on an
// idle instance and unusable on a busy one — which is the reported symptom, and
// which no amount of making the read itself cheaper would fix.
//
// The experiment runs an identical read mix twice: once alone, once against
// concurrent scheduler-journal appends, run-journal appends, and rollup ingest.
// It repeats that pair at each requested reader-concurrency level, reporting the
// levels separately, because "does a write load slow reads down" and "does it
// slow them down *more* as concurrency rises" are different questions and only
// the second distinguishes contention from a fixed per-read cost. Everything
// else is held constant. A large degradation confirms the mechanism; a small one
// refutes it and sends the investigation elsewhere, which §2.3 explicitly allows
// for — production evidence also includes refresh churn and proxy cancellations
// while direct endpoints were healthy.
//
// It deliberately uses the REAL journal.InstanceLog.Append rather than the
// generator's direct write. The generator bypasses it because it is O(n²) and
// cannot build a large journal in reasonable time — but that same method, taking
// a cross-process lock and re-reading the whole journal, is the write-path cost
// under test. Using the fast path here would remove the defect from the
// experiment designed to detect it.

// LoadSpec parameterizes the concurrent write load applied to the instance while
// reads are measured.
type LoadSpec struct {
	// Duration is how long to sustain the load, per reader-concurrency level.
	// §14.5 asks for a sustained run rather than a burst — "not a 60-second
	// burst" — because a burst can be absorbed by queueing that a sustained rate
	// cannot.
	Duration time.Duration
	// ReaderLevels are the reader-concurrency levels to measure, each one its own
	// idle/loaded pair. Concurrency is the axis the head-of-line-blocking claim
	// is about: a single fixed reader count reports one point on a curve and
	// cannot say whether degradation grows with the number of concurrent panels
	// or is flat in it, which is the difference between a contention mechanism
	// and a constant per-read cost.
	ReaderLevels []int
	// SchedulerAppendsPerSec is the rate of instance-journal appends via the real
	// InstanceLog.Append. On the live instance the scheduler journals a decision
	// per trigger evaluation, so this is not a synthetic load.
	SchedulerAppendsPerSec int
	// RunAppendsPerSec is the rate of run-journal appends across live runs.
	RunAppendsPerSec int
	// IngestPerSec is the rate of rollup ingests — the write path that shares the
	// store's single connection with every reader.
	IngestPerSec int
}

// DefaultLoadSpec is the §16.3 mixed load.
//
// The reader levels bracket the portal's several concurrent panels: one reader
// as the uncontended reference, four as the original fixed mix, and eight above
// it so a degradation that only appears past four is measured rather than
// missed.
func DefaultLoadSpec(d time.Duration) LoadSpec {
	return LoadSpec{
		Duration:               d,
		ReaderLevels:           []int{1, baselineReaders, 2 * baselineReaders},
		SchedulerAppendsPerSec: 5,
		RunAppendsPerSec:       10,
		IngestPerSec:           2,
	}
}

// baselineReaders is the fixed reader count the experiment ran at before it
// varied concurrency, kept as the middle level so a new run stays comparable to
// every baseline published under the old harness.
const baselineReaders = 4

// LoadResult reports every measured reader-concurrency level plus the
// store-bound read's worst degradation across them.
type LoadResult struct {
	Spec   LoadSpecReport    `json:"spec"`
	Levels []LoadLevelResult `json:"levels"`
	// StoreBoundOperation is the read the verdict keys on, and StoreBoundFactor
	// is its worst p99 degradation across the measured levels — worst, not the
	// last level's, because the hypothesis is that contention appears at *some*
	// concurrency, and averaging or taking only the top level would let a level
	// that confirmed it be reported as a level that did not.
	StoreBoundOperation string  `json:"storeBoundOperation"`
	StoreBoundFactor    float64 `json:"storeBoundFactor"`
	// StoreBoundReaders is the reader count at which StoreBoundFactor was
	// measured, so a factor is never quoted without the concurrency that produced
	// it.
	StoreBoundReaders int `json:"storeBoundReaders"`
}

// LoadLevelResult is one reader-concurrency level's measurement: the same read
// mix idle and under load, at that level.
type LoadLevelResult struct {
	Readers        int                `json:"readers"`
	Idle           map[string]Stat    `json:"idle"`
	UnderLoad      map[string]Stat    `json:"underLoad"`
	WritesApplied  WriteCounts        `json:"writesApplied"`
	Degradation    map[string]float64 `json:"degradationP99"`
	WorstOperation string             `json:"worstOperation"`
	WorstFactor    float64            `json:"worstFactor"`
	// StoreBoundFactor is the degradation of the read whose cost is dominated by
	// the shared SQLite connection rather than by the filesystem scan. It, not
	// WorstFactor, is the head-of-line-blocking signal — see storeBoundOp.
	StoreBoundFactor float64        `json:"storeBoundFactor"`
	Errors           map[string]int `json:"errors,omitempty"`
}

// LoadSpecReport is the spec as reported, with the duration in a readable form.
type LoadSpecReport struct {
	DurationSeconds        float64 `json:"durationSecondsPerLevel"`
	ReaderLevels           []int   `json:"readerLevels"`
	SchedulerAppendsPerSec int     `json:"schedulerAppendsPerSec"`
	RunAppendsPerSec       int     `json:"runAppendsPerSec"`
	IngestPerSec           int     `json:"ingestPerSec"`
}

// WriteCounts reports what the load actually managed to apply, which matters:
// if the writers could not sustain their target rate, the reads were measured
// against a lighter load than requested and the result understates degradation.
type WriteCounts struct {
	SchedulerAppends int64 `json:"schedulerAppends"`
	RunAppends       int64 `json:"runAppends"`
	Ingests          int64 `json:"ingests"`
	// SlowestSchedulerAppend is the worst single InstanceLog.Append observed.
	// §2.2 measures 1.30 s on the live 324 MB journal; this is the same number on
	// the generated corpus, and it is the cost #1914 removes.
	SlowestSchedulerAppendNanos time.Duration `json:"slowestSchedulerAppendNanos"`
}

// runMixedLoad measures the read mix idle, then again under concurrent writes,
// once per requested reader-concurrency level.
func runMixedLoad(
	layout instance.Layout,
	gen GenerateResult,
	spec LoadSpec,
	samples int,
) (LoadResult, error) {
	levels := normalizeReaderLevels(spec.ReaderLevels)
	if len(levels) == 0 {
		return LoadResult{}, fmt.Errorf(
			"scale: mixed load needs at least one positive reader-concurrency level; got %v", spec.ReaderLevels)
	}

	result := LoadResult{
		Spec: LoadSpecReport{
			DurationSeconds:        spec.Duration.Seconds(),
			ReaderLevels:           levels,
			SchedulerAppendsPerSec: spec.SchedulerAppendsPerSec,
			RunAppendsPerSec:       spec.RunAppendsPerSec,
			IngestPerSec:           spec.IngestPerSec,
		},
		StoreBoundOperation: storeBoundOp,
	}

	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		return LoadResult{}, fmt.Errorf("scale: open rollup: %w", err)
	}
	defer func() { _ = db.Close() }()

	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		return LoadResult{}, fmt.Errorf("scale: construct read service: %w", err)
	}

	mix := readMix(service, instancefixture.GaggleName(0))
	for _, readers := range levels {
		result.Levels = append(result.Levels, measureLoadLevel(mix, layout, gen, spec, readers, samples))
	}
	result.StoreBoundFactor, result.StoreBoundReaders = worstStoreBound(result.Levels)
	return result, nil
}

// normalizeReaderLevels drops non-positive levels, collapses duplicates, and
// sorts ascending so the report reads low to high and two runs of the harness
// line up level for level.
func normalizeReaderLevels(requested []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(requested))
	for _, n := range requested {
		if n < 1 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// worstStoreBound is the store-bound read's largest degradation across levels
// and the reader count that produced it. Zero when no level measured it, rather
// than a fabricated 1.0 that would read as "no degradation".
func worstStoreBound(levels []LoadLevelResult) (float64, int) {
	worst, at := 0.0, 0
	for _, level := range levels {
		if level.StoreBoundFactor > worst {
			worst, at = level.StoreBoundFactor, level.Readers
		}
	}
	return worst, at
}

// measureLoadLevel runs the three-phase measurement at one reader concurrency:
// idle, sustained load, idle again.
func measureLoadLevel(
	mix []readOp,
	layout instance.Layout,
	gen GenerateResult,
	spec LoadSpec,
	readers int,
	samples int,
) LoadLevelResult {
	level := LoadLevelResult{
		Readers:     readers,
		Idle:        map[string]Stat{},
		UnderLoad:   map[string]Stat{},
		Degradation: map[string]float64{},
		Errors:      map[string]int{},
	}

	// Warm-up, discarded. Without it the first phase absorbs every cold page-cache
	// and first-statement cost, and whichever phase runs first looks worse — which
	// on the first version of this experiment made reads appear 33% FASTER under
	// load and produced a false "not confirmed". A verdict that depends on
	// measurement order is not a verdict.
	_, _ = sampleMix(mix, readers*2, readers)

	// Phase 1: idle.
	idle1, errs := sampleMix(mix, samples, readers)
	mergeErrors(level.Errors, errs)

	// Phase 2: under sustained load. The mix is repeated until Duration elapses,
	// so this is a sustained measurement rather than a burst (§14.5) — an earlier
	// version accepted a Duration and then ran the load only for as long as one
	// pass of the mix took, which meant "30 minutes" and "5 seconds" measured the
	// same thing.
	stop := make(chan struct{})
	var writers sync.WaitGroup
	counts := &writeCounters{}
	startWriters(&writers, stop, counts, layout, gen, spec)
	time.Sleep(loadWarmup)

	underLoad, errs := sampleMixFor(mix, samples, readers, spec.Duration)
	level.UnderLoad = underLoad
	mergeErrors(level.Errors, errs)

	close(stop)
	writers.Wait()

	// Phase 3: idle again, after the load stops. Comparing against the *faster* of
	// the two idle phases is deliberately conservative — it makes degradation
	// harder to claim, which is the right bias when the output is a causal verdict
	// and drift (thermal, page cache, other tenants) is the obvious confounder.
	idle2, errs := sampleMix(mix, samples, readers)
	mergeErrors(level.Errors, errs)
	level.Idle = fasterOf(idle1, idle2)

	level.WritesApplied = WriteCounts{
		SchedulerAppends:            counts.schedulerAppends.Load(),
		RunAppends:                  counts.runAppends.Load(),
		Ingests:                     counts.ingests.Load(),
		SlowestSchedulerAppendNanos: time.Duration(counts.slowestSchedulerAppend.Load()),
	}

	// Degradation is reported at p99 rather than p50: head-of-line blocking is a
	// tail phenomenon. A read that waits behind a 1.3 s locked append is not
	// slower on average, it is occasionally catastrophic, and a p50 comparison
	// would miss exactly the effect under test.
	for op, before := range level.Idle {
		after, ok := level.UnderLoad[op]
		if !ok || before.P99 <= 0 {
			continue
		}
		factor := float64(after.P99) / float64(before.P99)
		level.Degradation[op] = factor
		if factor > level.WorstFactor {
			level.WorstFactor, level.WorstOperation = factor, op
		}
		if op == storeBoundOp {
			level.StoreBoundFactor = factor
		}
	}
	return level
}

// storeBoundOp names the read whose cost is dominated by the shared SQLite
// connection rather than by the filesystem.
//
// The distinction decides what this experiment can actually conclude, and it is
// not obvious. §2.3's hypothesis is specifically about contention on a single
// connection (SetMaxOpenConns(1)) shared by every reader and writer. But the
// inventory surfaces barely touch SQLite: their cost is the active-run scan
// opening one journal per run in history. So their degradation measures
// filesystem contention, and — measured — the write load *warms the page cache
// that same scan needs*, which makes them look FASTER under load and would
// otherwise be read as evidence against a hypothesis they cannot test.
//
// A bounded list page is the read that is genuinely store-bound, so it is the
// one whose degradation speaks to the connection-contention claim.
const storeBoundOp = opListRunsPage

// loadWarmup lets the writer goroutines reach their target rate before reads are
// sampled against them.
const loadWarmup = 500 * time.Millisecond

// fasterOf returns, per operation, whichever of two idle measurements had the
// lower p99 — the conservative baseline (see phase 3).
func fasterOf(a, b map[string]Stat) map[string]Stat {
	out := map[string]Stat{}
	for op, sa := range a {
		sb, ok := b[op]
		if !ok || sa.P99 <= sb.P99 {
			out[op] = sa
			continue
		}
		out[op] = sb
	}
	for op, sb := range b {
		if _, ok := out[op]; !ok {
			out[op] = sb
		}
	}
	return out
}

// sampleMixFor repeats the mix until at least d has elapsed, accumulating every
// sample. It is what makes the load *sustained*: the write goroutines keep
// running for as long as reads keep being issued, so a 30-minute run really
// applies 30 minutes of contention rather than one pass worth.
func sampleMixFor(mix []readOp, samples, readers int, d time.Duration) (map[string]Stat, map[string]int) {
	deadline := time.Now().Add(d)
	merged := map[string][]time.Duration{}
	errs := map[string]int{}
	for pass := 0; ; pass++ {
		out, passErrs := sampleMixRaw(mix, samples, readers)
		for op, ds := range out {
			merged[op] = append(merged[op], ds...)
		}
		mergeErrors(errs, passErrs)
		if time.Now().After(deadline) {
			break
		}
	}
	stats := map[string]Stat{}
	for op, ds := range merged {
		stats[op] = summarize(op, ds)
	}
	return stats, errs
}

// readOp is one named read the mix issues.
type readOp struct {
	name string
	fn   func() error
}

// readMix is the portal's read pattern: bounded list pages, the inventory
// surfaces, and run detail — the same operations measure() times, so idle
// numbers here and there are comparable.
func readMix(service *readservice.Local, gaggle string) []readOp {
	ctx := context.Background()
	return []readOp{
		{opListRunsPage, func() error {
			_, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50})
			return err
		}},
		{opInstance, func() error {
			_, err := service.Instance(ctx)
			return err
		}},
		{opGaggles, func() error {
			_, err := service.Gaggles(ctx, readservice.PageRequest{Limit: 50})
			return err
		}},
		{opWorkflows, func() error {
			_, err := service.Workflows(ctx, gaggle, readservice.PageRequest{Limit: 50})
			return err
		}},
	}
}

// sampleMix runs every operation `samples` times across `readers` concurrent
// goroutines and summarizes each. Concurrency is part of the measurement: §9's
// claim is that the Overview's several queries can run concurrently, and a
// serialized measurement cannot observe them failing to.
func sampleMix(mix []readOp, samples, readers int) (map[string]Stat, map[string]int) {
	raw, errs := sampleMixRaw(mix, samples, readers)
	out := map[string]Stat{}
	for op, ds := range raw {
		out[op] = summarize(op, ds)
	}
	return out, errs
}

// sampleMixRaw returns the individual durations rather than a summary, so a
// sustained run can pool samples across passes instead of averaging summaries.
func sampleMixRaw(mix []readOp, samples, readers int) (map[string][]time.Duration, map[string]int) {
	if readers < 1 {
		readers = 1
	}
	if samples < 2 {
		samples = 2
	}
	out := map[string][]time.Duration{}
	errs := map[string]int{}
	var mu sync.Mutex

	for _, op := range mix {
		durations := make([]time.Duration, 0, samples)
		var wg sync.WaitGroup
		per := samples / readers
		if per < 1 {
			per = 1
		}
		for r := 0; r < readers; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				local := make([]time.Duration, 0, per)
				failures := 0
				for i := 0; i < per; i++ {
					start := time.Now()
					err := op.fn()
					elapsed := time.Since(start)
					if err != nil {
						failures++
						continue
					}
					local = append(local, elapsed)
				}
				mu.Lock()
				durations = append(durations, local...)
				errs[op.name] += failures
				mu.Unlock()
			}()
		}
		wg.Wait()
		out[op.name] = durations
	}
	return out, errs
}

// writeCounters tallies what the load actually applied.
type writeCounters struct {
	schedulerAppends       atomic.Int64
	runAppends             atomic.Int64
	ingests                atomic.Int64
	slowestSchedulerAppend atomic.Int64
}

// startWriters launches the concurrent write load. Each writer ticks at its own
// rate and stops when stop closes; none fails the experiment on error, because a
// writer erroring under contention is itself a finding to report rather than a
// reason to abandon the measurement.
func startWriters(
	wg *sync.WaitGroup,
	stop <-chan struct{},
	counts *writeCounters,
	layout instance.Layout,
	gen GenerateResult,
	spec LoadSpec,
) {
	if spec.SchedulerAppendsPerSec > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			schedulerAppendLoad(stop, counts, layout, spec.SchedulerAppendsPerSec)
		}()
	}
	if spec.RunAppendsPerSec > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runAppendLoad(stop, counts, layout, gen, spec.RunAppendsPerSec)
		}()
	}
	if spec.IngestPerSec > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ingestLoad(stop, counts, layout, gen, spec.IngestPerSec)
		}()
	}
}

// schedulerAppendLoad appends to the instance journal through the production
// InstanceLog.Append — the method that takes a cross-process lock and re-reads
// the entire journal to allocate a sequence (§2.2). This is the write-path cost
// the experiment exists to observe competing with reads, so it must not be
// short-circuited.
func schedulerAppendLoad(stop <-chan struct{}, counts *writeCounters, layout instance.Layout, perSec int) {
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		return
	}
	defer func() { _ = log.Close() }()

	tick := time.NewTicker(time.Second / time.Duration(perSec))
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			start := time.Now()
			err := log.Append(journal.Event{
				Type:   journal.EventTickSkipped,
				Reason: "mixed-load: conditions not met",
			})
			elapsed := int64(time.Since(start))
			if err != nil {
				continue
			}
			counts.schedulerAppends.Add(1)
			for {
				prev := counts.slowestSchedulerAppend.Load()
				if elapsed <= prev || counts.slowestSchedulerAppend.CompareAndSwap(prev, elapsed) {
					break
				}
			}
		}
	}
}

// runAppendLoad appends heartbeats to existing run journals, modelling live runs
// progressing while the portal is read.
func runAppendLoad(stop <-chan struct{}, counts *writeCounters, layout instance.Layout, gen GenerateResult, perSec int) {
	if gen.Runs == 0 {
		return
	}
	tick := time.NewTicker(time.Second / time.Duration(perSec))
	defer tick.Stop()
	index := 0
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			// Every seventh generated run is left in flight, so those are the ones
			// that can legitimately take another event.
			runID := fmt.Sprintf("run-%08d", (index*7)%maxInt(gen.Runs, 1))
			index++
			if appendHeartbeat(layout, runID) == nil {
				counts.runAppends.Add(1)
			}
		}
	}
}

// appendHeartbeat opens one run and appends a stage heartbeat.
//
// Uses Layout.FindRunDir, which is the same resolution readservice.openRun
// performs, so the load exercises the production lookup across scoped and
// legacy roots rather than a harness-local guess at where a run lives.
func appendHeartbeat(layout instance.Layout, runID string) error {
	dir, err := layout.FindRunDir(runID)
	if err != nil {
		return err
	}
	run, _, err := journal.Recover(dir)
	if err != nil {
		return err
	}
	appendErr := run.Append(journal.Event{
		Type:    journal.EventStageHeartbeat,
		Stage:   instancefixture.StageName(0),
		Attempt: 1,
	})
	if closeErr := run.Close(); appendErr == nil {
		appendErr = closeErr
	}
	return appendErr
}

// ingestLoad re-ingests runs into the rollup, the write path that shares the
// store's single connection with every reader (§5.2).
func ingestLoad(stop <-chan struct{}, counts *writeCounters, layout instance.Layout, gen GenerateResult, perSec int) {
	if gen.Runs == 0 {
		return
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		return
	}
	defer func() { _ = db.Close() }()

	tick := time.NewTicker(time.Second / time.Duration(perSec))
	defer tick.Stop()
	index := 0
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			runID := fmt.Sprintf("run-%08d", index%maxInt(gen.Runs, 1))
			index++
			dir, err := layout.FindRunDir(runID)
			if err != nil {
				continue
			}
			if err := db.IngestRun(context.Background(), dir); err == nil {
				counts.ingests.Add(1)
			}
		}
	}
}

// mergeErrors folds one error tally into another.
func mergeErrors(dst, src map[string]int) {
	for k, v := range src {
		dst[k] += v
	}
}
