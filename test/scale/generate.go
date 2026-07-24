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
	"github.com/goobers/goobers/internal/journal"
)

// oversizedReason is the pathologically large field body injected into a handful
// of scheduler events so the read/ingest path is exercised against big records.
var oversizedReason = ": " + strings.Repeat("x", 64*1024)

// Baseline constants describe the current dogfood ("clubhouse") instance so a
// scale multiplier has a concrete referent (epic #1410): ~13.6k runs behind a
// ~290 MB scheduler journal. A Spec built from these via scaledSpec(mult)
// synthesizes an instance at mult× that size — 1× reproduces today's dogfood,
// 10×/100× are the resilience targets. They are deliberately approximate; the
// harness proves headroom, not an exact byte count.
const (
	baselineRuns            = 13_600
	baselineSchedulerEvents = 400_000
	baselineEventsPerRun    = 12
	baselineSpansPerRun     = 2
)

// gaggles is the fixed set of gaggle names the generator spreads runs across so
// gaggle-filtered read paths (ListRuns Gaggle filter, per-gaggle rollup) are
// exercised. Order is stable for deterministic run assignment.
var gaggles = []string{"goobers", "acme-web", "widget-service", "selfhost"}

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
	// EventsPerRun is roughly how many journal events each run accumulates
	// (stage attempts, heartbeats, refs) beyond its run.started/run.finished
	// bookends — the driver of per-run events.jsonl size.
	EventsPerRun int
	// SpansPerRun is how many content-addressed spans each run records under
	// spans/, exercising the span read/rollup path.
	SpansPerRun int
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
}

// scaledSpec builds a Spec at mult× the dogfood baseline. mult=1 reproduces the
// current instance; mult=10/100 are the epic #1410 resilience targets. Small
// per-run counts stay fixed — scale is dominated by run and scheduler-event
// counts, which is where the dogfood instance's cost actually lives.
func scaledSpec(root string, mult float64) Spec {
	scale := func(base int) int {
		n := int(float64(base) * mult)
		if n < 1 {
			return 1
		}
		return n
	}
	return Spec{
		Root:            root,
		Runs:            scale(baselineRuns),
		EventsPerRun:    baselineEventsPerRun,
		SpansPerRun:     baselineSpansPerRun,
		SchedulerEvents: scale(baselineSchedulerEvents),
		OrphanDirs:      5,
		OversizedRuns:   scale(baselineRuns) / 1000,
		Seed:            1,
	}
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
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		return GenerateResult{}, fmt.Errorf("scale: create runs dir: %w", err)
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
	}, nil
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
	gaggle := gaggles[index%len(gaggles)]
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
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 3,
		Gaggle:          gaggle,
		Trigger:         trigger,
		StartedAt:       clock,
	}, nil, journal.WithClock(tick))
	if err != nil {
		return fmt.Errorf("scale: create run %s: %w", runID, err)
	}

	// Spread EventsPerRun across stage attempts; each attempt is a
	// started/heartbeat/finished triple, so ~3 events per attempt.
	attempts := spec.EventsPerRun / 3
	if attempts < 1 {
		attempts = 1
	}
	for a := 1; a <= attempts; a++ {
		if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: a}); err != nil {
			return fmt.Errorf("scale: append stage.started %s: %w", runID, err)
		}
		if err := run.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "implement", Attempt: a}); err != nil {
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
			Type: journal.EventStageFinished, Stage: "implement", Attempt: a, AttemptClass: class, Status: status,
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

	for s := 0; s < spec.SpansPerRun; s++ {
		payload := fmt.Sprintf("synthetic transcript for %s span %d", runID, s)
		if _, err := run.RecordSpan("implement", fmt.Sprintf("transcript-%d", s), []byte(payload)); err != nil {
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

// terminalPhases is the spread of terminal outcomes assigned round-robin to
// finished runs so phase/outcome filters have every value to match.
var terminalPhases = []journal.RunPhase{
	journal.PhaseCompleted, journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseEscalated, journal.PhaseAborted,
}

// writeOrphanDir creates a runs/ subdirectory with no run.yaml — the pathology
// a crashed create or a half-pruned run leaves behind. rollup.runDirs and the
// read service's reconcile must skip these silently.
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
	return nil
}

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
	workflow := "implementation"
	gaggle := gaggles[index%len(gaggles)]
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
	if spec.SchedulerEvents >= 5000 && index%5000 == 0 {
		ev.Reason = ev.Reason + oversizedReason
	}
	return ev
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
