package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/instancefixture"
	"github.com/goobers/goobers/internal/journal"
)

// oversizedReason is the pathologically large field body injected into a handful
// of scheduler events so the read/ingest path is exercised against big records.
var oversizedReason = ": " + strings.Repeat("x", 64*1024)

// Baseline constants describe the live self-hosting instance so a scale
// multiplier has a concrete referent. A Spec built from these via
// scaledSpec(mult) synthesizes an instance at mult× that size — 1× reproduces
// the measured instance, 3×/10× are the resilience targets.
//
// # Provenance, and why these changed
//
// Measured on ~/source/goobers-instances on 2026-07-29 with the daemon down
// (design docs/design/portal-read-architecture.md §2). The previous constants
// (13,600 runs / 400,000 scheduler events) were written for epic #1410 against
// an earlier, smaller instance and had drifted badly: measured, -scale=1
// produced a 76 MiB scheduler journal against the live 324 MB, so every "1×"
// number understated the live instance by roughly 4× on journal bytes. Design
// §2.5 records the correction.
//
// These are dated on purpose. The live instance grows, so this baseline will
// drift again; re-anchor it deliberately with a new measurement rather than
// letting "1×" quietly stop meaning anything.
//
//	Run directories                    40,665
//	  …published (have run.yaml)       29,759
//	  …unpublished (no run.yaml)       10,906  (26.8%)
//	Run events.jsonl total             191 MB
//	  mean / p50 / p90 / p99 / max     6.7 KB / 6.0 / 13.5 / 39.8 / 131 KB
//	Span files                         70,425 totalling 2,263 MB (~32 KB each)
//	Scheduler journal                  324 MB across 156,765 records
const (
	// baselineRuns is *published* run directories — the ones with a run.yaml,
	// which are the only ones the read path can index. Unpublished directories
	// are generated separately via OrphanDirs, so the total directory count at
	// 1× is baselineRuns + orphans ≈ 40,665.
	baselineRuns = 29_759
	// baselineSchedulerEvents is the record count, not a byte target. See
	// GiantSchedulerRecords for why the 324 MB is not modelled as an average.
	baselineSchedulerEvents = 156_765
	// baselineOrphanFraction is 10,906 / 40,665. Expressed as a fraction of the
	// published run count because a *fixed* count (previously 5, at any scale)
	// is not a pathology at 100k runs — it rounds to nothing. 26.8% of
	// directories being unindexable is a load-bearing property of the live
	// instance, and it is what makes a directory sweep expensive.
	baselineOrphanFraction = 10_906.0 / 29_759.0
	// baselineSpanBytes targets the measured mean span payload:
	// 2,263 MB / 70,425 files ≈ 32 KB. The generator previously wrote ~45-byte
	// payloads, which inverted the live span:event byte ratio (2,263 MB of spans
	// against 191 MB of events, ~11×) by several orders of magnitude — and that
	// ratio is the entire argument for §5.1's two-store split, so a harness that
	// inverts it cannot test the decision it is meant to justify.
	baselineSpanBytes = 32 * 1024
	// baselineSpansPerRun is the integer floor of the measured mean; the
	// fractional remainder is applied by spansForRun so the corpus reaches the
	// real 2.37 spans/run rather than truncating 15% of the span footprint away.
	baselineSpansPerRun = 2
	// baselineExtraSpanFraction is that remainder: 70,425 / 29,759 = 2.366, so
	// 36.6% of runs carry one more span than the floor.
	baselineExtraSpanFraction = (70_425.0 / 29_759.0) - baselineSpansPerRun
)

// spansForRun returns how many spans this run records, applying
// ExtraSpanFraction deterministically so the corpus mean matches the measured
// 2.37 spans/run instead of the truncated integer.
func spansForRun(rng *rand.Rand, spec Spec) int {
	n := spec.SpansPerRun
	if spec.ExtraSpanFraction > 0 && rng.Float64() < spec.ExtraSpanFraction {
		n++
	}
	return n
}

// runEventCountQuantiles maps a uniform draw in [0,1) to a per-run journal event
// count, calibrated so generated events.jsonl sizes reproduce the live
// instance's measured distribution rather than its mean.
//
// This replaces a constant EventsPerRun, and the difference is not cosmetic.
// Measured on a generated corpus, the old generator produced p50 == p90 == p99
// == max == 3,050 bytes: **every run cost exactly the same to parse.** The live
// instance's p99 is 6.6× its p50 and its max is 22× its p50. A harness with no
// tail cannot exercise the single-run cost class (§7.1), cannot reproduce the
// §2.3 measurement that ruled out "one huge event ledger" as the run-detail
// timeout cause, and will report a flattering p99.9 for any per-run read path.
//
// Counts are derived from the measured byte quantiles at the generator's
// ~217 bytes per event; the resulting size distribution is asserted against the
// live quantiles in TestGeneratedEventSizesMatchLiveDistribution.
var runEventCountQuantiles = []struct {
	q      float64
	events int
}{
	{0.00, 8},   // floor: bookends plus one attempt
	{0.50, 28},  // 6.0 KB
	{0.90, 62},  // 13.5 KB
	{0.99, 183}, // 39.8 KB
	{1.00, 604}, // 131 KB — the largest event log in 40,665 runs
}

// eventsForRun draws a per-run event count from runEventCountQuantiles by
// linear interpolation. The draw is deterministic in the run index (not in
// worker scheduling order), so a Spec still reproduces byte-for-byte.
func eventsForRun(rng *rand.Rand, spec Spec) int {
	// An explicit EventsPerRun pins every run to one size — kept because the
	// correctness tests and the oversized-record fixtures want a fixed, cheap
	// corpus, and because pinning it is the only way to isolate a change's
	// effect from the distribution's variance.
	if spec.EventsPerRun > 0 {
		return spec.EventsPerRun
	}
	u := rng.Float64()
	for i := 1; i < len(runEventCountQuantiles); i++ {
		hi := runEventCountQuantiles[i]
		if u > hi.q {
			continue
		}
		lo := runEventCountQuantiles[i-1]
		span := hi.q - lo.q
		if span <= 0 {
			return hi.events
		}
		frac := (u - lo.q) / span
		return lo.events + int(frac*float64(hi.events-lo.events))
	}
	return runEventCountQuantiles[len(runEventCountQuantiles)-1].events
}

// gaggleNames returns the gaggle names runs are spread across, taken from the
// inventory spec so runs and definitions agree.
//
// They must be the same names. A generator with its own list — this one used
// {"goobers", "acme-web", "widget-service", "reference-workflows"} while the inventory
// declared none at all — produces runs whose gaggle no definition mentions, and
// then every inventory surface reports zero active runs *while looking perfectly
// healthy*. That is the worst kind of harness bug: it makes the measurement
// meaningless without making it fail.
func gaggleNames(spec Spec) []string {
	n := spec.Inventory.Gaggles
	if n < 1 {
		n = 1
	}
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		names = append(names, instancefixture.GaggleName(i))
	}
	return names
}

// Spec is the parameterizable shape of a synthetic instance. The zero value is
// not useful; build one with defaultSpec or scaledSpec and override fields as
// needed. Every field is a knob the load harness turns to move between 1× and
// 100× the dogfood instance.
type Spec struct {
	// Root is the instance root directory the generator populates. It is
	// created if absent; an instance.Layout is derived from it so runs, the
	// scheduler journal, and telemetry.db land where the real daemon puts them.
	Root string
	// Runs is the number of run directories to synthesize, each a valid
	// run.yaml + events.jsonl written through the production journal API.
	Runs int
	// EventsPerRun pins every run to this many journal events (stage attempts,
	// heartbeats, refs) beyond its run.started/run.finished bookends. **Zero
	// means draw from runEventCountQuantiles instead**, reproducing the live
	// instance's heavy-tailed size distribution — which is what scaledSpec does.
	// Pin it only when a fixed, cheap, zero-variance corpus is what you want.
	EventsPerRun int
	// SpansPerRun is how many content-addressed spans each run records under
	// spans/, exercising the span read/rollup path. It is the floor; see
	// ExtraSpanFraction.
	SpansPerRun int
	// ExtraSpanFraction is the fraction of runs that record one span beyond
	// SpansPerRun, so a non-integer measured mean (2.37 on the live instance) can
	// be reproduced rather than truncated. Zero keeps every run at the floor.
	ExtraSpanFraction float64
	// SpanBytes is the approximate payload size of each generated span. It
	// exists because the span:event byte ratio (~12× on the live instance) is
	// the premise of the §5.1 two-store split, and a harness that gets it wrong
	// cannot measure the split's benefit. Zero falls back to a short
	// human-readable payload, which is what the cheap correctness fixtures want.
	SpanBytes int
	// GiantSchedulerRecords injects this many pathologically large records into
	// the scheduler journal.
	//
	// This models a measured property that an average cannot: **88.8% of the
	// live 324 MB journal is 287 MB across just 108 records** (max 2.66 MB),
	// residue of the already-fixed #1414 unbounded-aggregate defect. Modelling
	// the journal as 324 MB / 156,765 records would give every record ~2 KB and
	// reproduce neither the byte total's cause nor the tail that Wave 1.1's
	// backward scan and byte budget have to survive — the budget must sit above
	// the largest real record or the full-recovery path fires on ordinary data.
	GiantSchedulerRecords int
	// SchedulerEvents is the number of instance-level events written to
	// scheduler/events.jsonl. Written directly (not via InstanceLog.Append,
	// which re-reads the whole file per append — O(n²)) so a multi-hundred-MB
	// journal is generatable in linear time.
	SchedulerEvents int
	// OrphanDirs injects this many directories under runs/ that contain no
	// run.yaml — the resilience pathology a crashed/partially-created run
	// leaves behind. The read and rollup paths must skip them, not fail.
	OrphanDirs int
	// OversizedRuns injects an oversized artifact into this many runs (a large
	// content-addressed blob + its artifact.recorded event), exercising the
	// read path against pathologically large records.
	OversizedRuns int
	// Seed makes the run/label distribution deterministic so a given Spec
	// reproduces byte-for-byte across machines and runs.
	Seed int64
	// Inventory describes the gaggle/workflow definitions generated alongside the
	// runs. It is part of the Spec rather than supplied separately at measure time
	// because the runs must be attributed to the gaggles and workflows the
	// definitions declare — see gaggleNames for what goes wrong when they diverge.
	Inventory instancefixture.InventorySpec
	// FlatRunsDir writes every run into the legacy single <root>/runs directory
	// instead of per-gaggle roots.
	//
	// Per-gaggle is the default because it is what the live instance uses: its
	// root `runs` is a *symlink* to `gaggles/goobers/runs`, and Layout.RunDirs
	// deliberately skips that symlink and enumerates `gaggles/*/runs` instead. A
	// harness on the flat layout therefore exercises a different code path from
	// production — one run root instead of N — and cannot measure the multi-root
	// walk that ActiveRunCountsByWorkflowDirs actually performs.
	FlatRunsDir bool
}

// scaledSpec builds a Spec at mult× the measured live instance. mult=1
// reproduces it; mult=3/10 are the resilience targets (design §16). Per-run
// event counts are drawn from the measured distribution rather than fixed
// (EventsPerRun stays 0), because scale is dominated by run and scheduler-event
// counts while *tail* behavior is dominated by the per-run distribution — and
// both matter.
func scaledSpec(root string, mult float64) Spec {
	scale := func(base int) int {
		n := int(float64(base) * mult)
		if n < 1 {
			return 1
		}
		return n
	}
	runs := scale(baselineRuns)
	return Spec{
		Root:              root,
		Runs:              runs,
		EventsPerRun:      0, // draw from runEventCountQuantiles
		SpansPerRun:       baselineSpansPerRun,
		ExtraSpanFraction: baselineExtraSpanFraction,
		SpanBytes:         baselineSpanBytes,
		SchedulerEvents:   scale(baselineSchedulerEvents),
		// Proportional, not fixed: 26.8% of live directories are unpublished,
		// and a fixed count stops being a pathology the moment scale grows.
		OrphanDirs:    int(float64(runs) * baselineOrphanFraction),
		OversizedRuns: runs / 1000,
		// 108 giant records at 1×, scaled — the #1414 residue that dominates the
		// live journal's byte count.
		GiantSchedulerRecords: scaleGiantRecords(scale(baselineSchedulerEvents)),
		Seed:                  1,
		// The inventory is scaled too, because §14.4's fan-out property is stated
		// in workflow count: "a page showing W workflows issues a request count
		// that does not grow with W". At 1× that is baselineWorkflows; the design
		// asks for 2,000 at the fan-out assertion, which -workflows pins directly.
		Inventory: instancefixture.InventorySpec{
			InstanceName:      "scale-harness",
			Gaggles:           baselineGaggles,
			Workflows:         scaleWorkflows(mult),
			GoobersPerGaggle:  baselineGoobersPerGaggle,
			TasksPerWorkflow:  baselineTasksPerWorkflow,
			MaxConcurrentRuns: 4,
		},
	}
}

// Inventory baseline constants. The live instance runs a single gaggle with a
// handful of workflows, but a single gaggle cannot exercise the gaggle-leading
// authorization predicate (§5.5) or the multi-root walk that
// ActiveRunCountsByWorkflowDirs performs, so the baseline declares several.
const (
	baselineGaggles          = 4
	baselineWorkflows        = 40
	baselineGoobersPerGaggle = 3
	baselineTasksPerWorkflow = 3
)

// scaleWorkflows grows the workflow count with the multiplier but keeps it
// bounded: workflow count drives definition-inventory cost, which is
// independent of run history, so scaling it linearly with runs would conflate
// two dimensions the harness needs to separate.
func scaleWorkflows(mult float64) int {
	n := int(float64(baselineWorkflows) * mult)
	if n < 1 {
		return 1
	}
	return n
}

// baselineGiantSchedulerRecords is the measured count of #1414-residue records on
// the live instance: 108 records holding 287 MB of a 324 MB journal.
const baselineGiantSchedulerRecords = 108

// scaleGiantRecords derives the giant-record count from a scheduler event count,
// holding the live instance's ratio (108 per 156,765 records). Derived rather
// than passed so pinning -scheduler-events cannot leave the pathology behind at
// a stale absolute count.
func scaleGiantRecords(schedulerEvents int) int {
	if schedulerEvents <= 0 {
		return 0
	}
	n := schedulerEvents * baselineGiantSchedulerRecords / baselineSchedulerEvents
	if n < 1 {
		// Below ~1,450 events the ratio rounds to zero. Keep one so even a small
		// corpus exercises the large-record path the byte budget has to survive.
		return 1
	}
	return n
}

// GenerateResult reports what a generation run produced — the on-disk footprint
// the read-path benchmarks then measure against.
type GenerateResult struct {
	Layout               instance.Layout
	Runs                 int
	OrphanDirs           int
	SchedulerEvents      int
	SchedulerJournalSize int64
	Elapsed              time.Duration
	// Inventory is the definition spec the runs were attributed to, carried
	// through so the measurement builds definitions that match the corpus.
	Inventory instancefixture.InventorySpec
	// Gaggles are the run roots' gaggle names, in generation order.
	Gaggles []string
}

// generate synthesizes the instance described by spec, writing every run
// through the production journal.Create/Append/Record* API so the on-disk
// format tracks schema evolution automatically (a format change that breaks the
// daemon breaks this harness too, by construction). The scheduler journal is
// written directly in the canonical envelope to sidestep InstanceLog.Append's
// per-append full re-read. It is deterministic given spec.Seed.
func generate(spec Spec) (GenerateResult, error) {
	if spec.Runs < 0 || spec.SchedulerEvents < 0 {
		return GenerateResult{}, fmt.Errorf("scale: runs and scheduler events must be non-negative")
	}
	start := time.Now()
	layout := instance.NewLayout(spec.Root)
	// One run root per gaggle, matching the live instance's layout (see
	// Spec.FlatRunsDir). Every root is created up front so a worker never races
	// another on MkdirAll.
	names := gaggleNames(spec)
	for _, name := range names {
		if err := os.MkdirAll(runsDirFor(layout, spec, name), 0o755); err != nil {
			return GenerateResult{}, fmt.Errorf("scale: create runs dir for %s: %w", name, err)
		}
	}
	// A fixed epoch keeps StartedAt deterministic; runs march forward one
	// minute apart so newest-first ordering and time-window filters have a real
	// spread to sort and slice.
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Generation is dominated by the journal's per-append fsync, which is
	// latency-bound, not CPU-bound. Each run is an independent directory, so a
	// pool of workers oversubscribing the cores lets the fsync latencies overlap
	// and cuts wall time by roughly the worker count. Determinism is preserved:
	// every run's identity, labels, and content derive only from its index (and
	// a per-run rng seeded from index), never from scheduling order.
	if err := generateRunsParallel(layout, spec, epoch); err != nil {
		return GenerateResult{}, err
	}
	for i := 0; i < spec.OrphanDirs; i++ {
		if err := writeOrphanDir(layout, i); err != nil {
			return GenerateResult{}, err
		}
	}
	schedSize, err := writeSchedulerJournal(layout, spec, epoch)
	if err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{
		Layout:               layout,
		Runs:                 spec.Runs,
		OrphanDirs:           spec.OrphanDirs,
		SchedulerEvents:      spec.SchedulerEvents,
		SchedulerJournalSize: schedSize,
		Elapsed:              time.Since(start),
		Inventory:            spec.Inventory,
		Gaggles:              names,
	}, nil
}

// runsDirFor returns the run root a given gaggle's runs belong in.
func runsDirFor(layout instance.Layout, spec Spec, gaggle string) string {
	if spec.FlatRunsDir {
		return layout.RunsDir()
	}
	return layout.ForGaggle(gaggle).RunsDir()
}

// generateRunsParallel fans run generation across a bounded worker pool. The
// pool size oversubscribes the cores because the work is fsync-latency-bound;
// concurrent journal.Create calls are safe (each takes its own per-run-dir
// lock, #243). The first worker error cancels the rest and is returned.
func generateRunsParallel(layout instance.Layout, spec Spec, epoch time.Time) error {
	workers := runtime.NumCPU() * 4
	if workers > spec.Runs {
		workers = spec.Runs
	}
	if workers < 1 {
		return nil
	}
	indexes := make(chan int, workers)
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
		failed   atomic.Bool
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A per-run rng keyed off the seed and index keeps oversized-record
			// content deterministic regardless of which worker draws the run.
			// Workers keep draining after a failure (cheaply skipping remaining
			// runs) so the feeder never blocks on a full channel — only the
			// first error is retained.
			for i := range indexes {
				if failed.Load() {
					continue
				}
				rng := rand.New(rand.NewSource(spec.Seed + int64(i)))
				if err := generateRun(layout, spec, rng, i, epoch); err != nil {
					errOnce.Do(func() { firstErr = err })
					failed.Store(true)
				}
			}
		}()
	}
	for i := 0; i < spec.Runs; i++ {
		indexes <- i
	}
	close(indexes)
	wg.Wait()
	return firstErr
}

// generateRun writes one realistic run journal: run.started (implicit), a spread
// of stage attempts with heartbeats and refs, optional spans and an oversized
// artifact, then a terminal run.finished for most runs (every seventh is left
// in flight so the "running" phase is represented). Its per-run clock advances
// monotonically so LastActivityAt and durations are meaningful.
func generateRun(layout instance.Layout, spec Spec, rng *rand.Rand, index int, epoch time.Time) error {
	runID := fmt.Sprintf("run-%08d", index)
	names := gaggleNames(spec)
	gaggle := names[index%len(names)]
	// Attribute the run to a workflow the inventory actually declares, so
	// LatestPerWorkflow and the workflow-detail surface have matching data
	// instead of resolving to nothing.
	workflowName := instancefixture.WorkflowName(index % maxInt(spec.Inventory.Workflows, 1))
	stageName := instancefixture.StageName(0)
	trigger := journal.Trigger{Kind: journal.TriggerSchedule, Ref: "0 * * * *"}
	if index%3 == 0 {
		trigger = journal.Trigger{Kind: journal.TriggerManual}
	}

	// One clock per run, closed over by WithClock, so every event this run
	// appends is stamped deterministically without touching the wall clock.
	clock := epoch.Add(time.Duration(index) * time.Minute)
	tick := func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	run, err := journal.Create(runsDirFor(layout, spec, gaggle), journal.RunIdentity{
		RunID:           runID,
		Workflow:        workflowName,
		WorkflowVersion: 3,
		Gaggle:          gaggle,
		Trigger:         trigger,
		StartedAt:       clock,
	}, nil, journal.WithClock(tick))
	if err != nil {
		return fmt.Errorf("scale: create run %s: %w", runID, err)
	}

	// Spread this run's event budget across stage attempts; each attempt is a
	// started/heartbeat/finished triple, so ~3 events per attempt. The budget is
	// drawn from the measured live distribution unless EventsPerRun pins it, so
	// the corpus has the long tail the live instance has (see eventsForRun).
	attempts := eventsForRun(rng, spec) / 3
	if attempts < 1 {
		attempts = 1
	}
	for a := 1; a <= attempts; a++ {
		if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: stageName, Attempt: a}); err != nil {
			return fmt.Errorf("scale: append stage.started %s: %w", runID, err)
		}
		if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: stageName, Attempt: a}); err != nil {
			return fmt.Errorf("scale: append stage.heartbeat %s: %w", runID, err)
		}
		status := "success"
		class := journal.AttemptClass("")
		if a < attempts {
			// Non-final attempts model policy retries so retry counts are non-trivial.
			status = "failure"
			class = journal.AttemptPolicy
		}
		if err := run.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: stageName, Attempt: a, AttemptClass: class, Status: status,
		}); err != nil {
			return fmt.Errorf("scale: append stage.finished %s: %w", runID, err)
		}
	}

	if err := run.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: fmt.Sprintf("%d", 1000+index)},
	}); err != nil {
		return fmt.Errorf("scale: append ref.touched %s: %w", runID, err)
	}

	for s, n := 0, spansForRun(rng, spec); s < n; s++ {
		if _, err := run.RecordSpan(stageName, fmt.Sprintf("transcript-%d", s), spanPayload(runID, s, spec.SpanBytes)); err != nil {
			return fmt.Errorf("scale: record span %s: %w", runID, err)
		}
	}

	// Oversized-record pathology: a large content-addressed artifact on a few
	// runs, to prove the read path stays bounded against big records.
	if spec.OversizedRuns > 0 && index%maxInt(spec.Runs/spec.OversizedRuns, 1) == 0 && index/maxInt(spec.Runs/spec.OversizedRuns, 1) < spec.OversizedRuns {
		big := make([]byte, 512*1024)
		for i := range big {
			big[i] = byte('a' + rng.Intn(26))
		}
		if _, err := run.RecordArtifact("oversized-diff.txt", big); err != nil {
			return fmt.Errorf("scale: record oversized artifact %s: %w", runID, err)
		}
	}

	// Leave every seventh run in flight so the "running" phase is exercised;
	// finish the rest with a spread of terminal phases.
	if index%7 == 0 {
		return run.Close()
	}
	phase := terminalPhases[index%len(terminalPhases)]
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		return fmt.Errorf("scale: append run.finished %s: %w", runID, err)
	}
	return run.Close()
}

// spanPayload builds a span body of approximately size bytes. Spans are
// content-addressed, so the body must differ per (run, span) or every span
// collapses to one blob on disk and the span store's real footprint — 2,263 MB
// across 70,425 files on the live instance — is never reproduced.
//
// A size of 0 yields the short human-readable payload the cheap correctness
// fixtures want.
func spanPayload(runID string, index, size int) []byte {
	head := fmt.Sprintf("synthetic transcript for %s span %d\n", runID, index)
	if size <= len(head) {
		return []byte(head)
	}
	body := make([]byte, 0, size)
	body = append(body, head...)
	// Repeating a per-span-unique line keeps the content distinct (so digests
	// differ) while staying cheap to generate.
	line := fmt.Sprintf("%s:%d transcript line padding to reach the measured mean span size\n", runID, index)
	for len(body) < size {
		body = append(body, line...)
	}
	return body[:size]
}

// terminalPhases is the spread of terminal outcomes assigned round-robin to
// finished runs so phase/outcome filters have every value to match.
var terminalPhases = []journal.RunPhase{
	journal.PhaseCompleted, journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseEscalated, journal.PhaseAborted,
}

// writeOrphanDir creates a runs/ subdirectory with no run.yaml — the pathology
// a crashed create or a half-pruned run leaves behind. rollup.runDirs and the
// read service's reconcile must skip these silently.
//
// Each orphan also gets a `.lock` file, which is the single most important
// detail in this function and was previously **inverted**: the generator wrote
// orphans without one while every journal.Create'd run had one, and the live
// instance is the exact opposite — all 10,906 unpublished directories contain a
// `.lock` (design §2.4). Those locks are what proved a read path was performing
// maintenance on directories it could never ingest, and reproducing them is what
// lets the harness assert the §14.1 property that no read path creates one. A
// corpus with the polarity reversed asserts nothing.
func writeOrphanDir(layout instance.Layout, index int) error {
	dir := filepath.Join(layout.RunsDir(), fmt.Sprintf("orphan-%04d", index))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("scale: create orphan dir: %w", err)
	}
	// A stray file with no run.yaml is the realistic shape — a run dir whose
	// creation died before pinning its identity.
	if err := os.WriteFile(filepath.Join(dir, "state.json.tmp"), []byte("{}"), 0o644); err != nil {
		return fmt.Errorf("scale: write orphan stray file: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, orphanLockFileName), nil, 0o644); err != nil {
		return fmt.Errorf("scale: write orphan lock file: %w", err)
	}
	return nil
}

// orphanLockFileName mirrors journal's unexported fileLock. Duplicated rather
// than exported: the name is part of the on-disk pathology this harness
// reproduces, not part of journal's API, and exporting it to satisfy a test
// would widen a package boundary for no other caller. If journal ever renames
// it, TestOrphanDirsCarryLockFiles fails loudly rather than silently generating
// a corpus that no longer matches the instance.
const orphanLockFileName = ".lock"

// writeSchedulerJournal writes spec.SchedulerEvents instance-level events
// directly to scheduler/events.jsonl in the canonical journal envelope. It
// bypasses journal.OpenInstanceLog.Append deliberately: that method re-reads the
// entire log on every append to recompute seq, which is O(n²) and cannot build a
// multi-hundred-MB journal in reasonable time. The bytes it writes are the exact
// envelope InstanceLog would produce (schema, seq, branch, time, type), so the
// rollup's scheduler ingest reads them identically. Returns the journal size.
func writeSchedulerJournal(layout instance.Layout, spec Spec, epoch time.Time) (int64, error) {
	dir := layout.SchedulerDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("scale: create scheduler dir: %w", err)
	}
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("scale: create scheduler journal: %w", err)
	}
	// A large buffer keeps the many small line writes from becoming many small
	// syscalls; a multi-hundred-MB journal is write-bound otherwise.
	w := bufio.NewWriterSize(f, 1<<20)
	for i := 0; i < spec.SchedulerEvents; i++ {
		ev := schedulerEvent(spec, i, epoch)
		line, err := json.Marshal(ev)
		if err != nil {
			_ = f.Close()
			return 0, fmt.Errorf("scale: marshal scheduler event: %w", err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			_ = f.Close()
			return 0, fmt.Errorf("scale: write scheduler event: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("scale: flush scheduler journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("scale: close scheduler journal: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("scale: stat scheduler journal: %w", err)
	}
	return info.Size(), nil
}

// schedulerEvent builds one instance-journal event across the scheduler
// taxonomy, with seq assigned monotonically from 1. Every few thousand records
// carries an oversized Reason to inject the oversized-record pathology into the
// scheduler journal too.
func schedulerEvent(spec Spec, index int, epoch time.Time) journal.Event {
	seq := uint64(index + 1)
	ev := journal.Event{
		Schema: journal.EventSchema,
		Seq:    seq,
		Time:   epoch.Add(time.Duration(index) * time.Second),
	}
	names := gaggleNames(spec)
	workflow := instancefixture.WorkflowName(index % maxInt(spec.Inventory.Workflows, 1))
	gaggle := names[index%len(names)]
	switch index % 5 {
	case 0:
		ev.Type = journal.EventTriggerFired
		ev.Workflow, ev.Gaggle, ev.Reason = workflow, gaggle, "scheduled"
	case 1:
		ev.Type = journal.EventTickSkipped
		ev.Workflow, ev.Gaggle, ev.Reason = workflow, gaggle, "conditions: max-parallel"
	case 2:
		ev.Type = journal.EventRunStarted
		ev.Workflow, ev.Gaggle, ev.RunID = workflow, gaggle, fmt.Sprintf("run-%08d", index%maxInt(spec.Runs, 1))
	case 3:
		ev.Type = journal.EventRunFinished
		ev.Workflow, ev.Gaggle, ev.RunID, ev.Status = workflow, gaggle, fmt.Sprintf("run-%08d", index%maxInt(spec.Runs, 1)), "completed"
	default:
		ev.Type = journal.EventClaimAcquired
		ev.RunID = fmt.Sprintf("run-%08d", index%maxInt(spec.Runs, 1))
	}
	// The 64 KiB oversized-record pathology: frequent, moderate, and pre-existing.
	if spec.SchedulerEvents >= 5000 && index%5000 == 0 {
		ev.Reason = ev.Reason + oversizedReason
	}
	// The #1414 residue: rare, enormous, and where the live journal's byte count
	// actually lives — 108 records holding 88.8% of 324 MB, max 2.66 MB. Spread
	// evenly so a backward tail scan is realistically likely to meet one.
	//
	// Shape taken from the live records rather than invented: type "error", with
	// the bytes in error.message and a consecutiveFailures runner annotation.
	// Verified against the instance — all 108 are type "error".
	if n := spec.GiantSchedulerRecords; n > 0 {
		if stride := maxInt(spec.SchedulerEvents/n, 1); index%stride == 0 && index/stride < n {
			ev.Type = journal.EventError
			ev.Error = &journal.ErrorDetail{
				Code:    "stalled_run_sweep_failed",
				Message: strings.Repeat("y", giantSchedulerRecordBytes),
			}
			ev.Runner = map[string]any{"consecutiveFailures": 1}
		}
	}
	return ev
}

// giantSchedulerRecordBytes is the measured maximum instance-journal record on
// the live instance (2,661,279 bytes, #1414 residue), rounded down. Wave 1.1's
// bounded backward scan needs a byte budget **above** this or its
// full-recovery fallback fires on ordinary history, so the harness has to be
// able to produce one.
const giantSchedulerRecordBytes = 2_600_000

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
