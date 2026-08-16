package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/instancefixture"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readprobe"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// Measurement is one scale point's cost across the telemetry read/ingest paths
// the portal depends on. Sizes are on-disk bytes after ingest; every latency is
// a Stat with cold and warm figures reported separately (design §14.12).
//
// Before this it held one wall-clock duration per operation, taken once. A
// single sample cannot support a p99.9 target, cannot distinguish a regression
// from scheduler noise, and made every §14.12 comparison a coin flip — so the
// operations are now sampled repeatedly and summarized.
type Measurement struct {
	Host                 Host  `json:"host"`
	Runs                 int   `json:"runs"`
	OrphanDirs           int   `json:"orphanDirs"`
	SchedulerEvents      int   `json:"schedulerEvents"`
	SchedulerJournalSize int64 `json:"schedulerJournalBytes"`
	TelemetryDBSize      int64 `json:"telemetryDBBytes"`
	RunEventsSize        int64 `json:"runEventsBytes"`
	SpansSize            int64 `json:"spansBytes"`

	// RollupRebuild is the cost of rollup.Rebuild — the full re-ingest of every
	// run journal plus the scheduler log into telemetry.db (the `--rebuild` and
	// cold-start path). Sampled once: it is destructive and minutes long, so
	// repeating it would dominate the harness's own runtime.
	RollupRebuild time.Duration `json:"rollupRebuildNanos"`

	// Stats holds the repeatedly-sampled read paths, keyed by operation.
	Stats []Stat `json:"stats"`

	// RebuildOpens is how many run journals the rebuild had to open, and
	// SimulatedBlobRebuild projects what that rebuild would additionally cost on
	// a mount with per-open latency.
	//
	// §2.6 concludes the rebuild cost driver is "file opens, not bytes" — 29,759
	// of them on the live instance — and §13.2's per-replica-versus-shared
	// decision turns on that number over a network mount, for which the design
	// says "no defensible figure exists". This produces one, clearly labelled as
	// a projection rather than a measurement.
	RebuildOpens         uint64        `json:"rebuildJournalOpens"`
	SimulatedBlobRebuild time.Duration `json:"simulatedBlobRebuildNanos"`
	BlobPerOpenLatency   time.Duration `json:"blobPerOpenLatencyNanos"`

	// MixedLoad, when present, is the §16.3 experiment's result.
	MixedLoad *LoadResult `json:"mixedLoad,omitempty"`

	// Work is the expensive work each operation performs for ONE invocation,
	// keyed by operation name. Latency alone cannot distinguish "fast because
	// bounded" from "fast because the corpus is small", which is how a read path
	// regresses to O(history) with every timing test still green (§14.2).
	Work map[string]readprobe.Snapshot `json:"work"`
}

// dashboardPhases mirror the phases the Overview fans out over, one ListRuns
// each — the read pattern the overview_fanout stat times.
var dashboardPhases = []journal.RunPhase{
	journal.PhaseRunning,
	journal.PhaseCompleted,
	journal.PhaseFailed,
	journal.PhaseEscalated,
	journal.PhaseAborted,
}

// Operation names, used as both the report labels and the JSON keys, so a
// baseline artifact stays comparable across harness versions.
const (
	opListRunsPage     = "listruns_page"
	opOverviewFanout   = "overview_fanout"
	opStatusFullScan   = "status_full_scan"
	opListRunsGaggle   = "listruns_gaggle_filtered"
	opListRunsDeepPage = "listruns_deep_page"

	// The inventory surfaces. Every one of these reaches the active-run scan
	// that measures 17.2 s cold on the live instance to answer "2" (design
	// §2.1), and **none of them was measured here before** — which is why five
	// patches to the read path could each be correct about a mechanism while none
	// of them changed the shape. §2.5 records the gap.
	opInstance          = "instance"
	opGaggles           = "gaggles"
	opWorkflows         = "workflows"
	opWorkflowDetail    = "workflow_detail"
	opLatestPerWorkflow = "listruns_latest_per_workflow"

	// Run detail as the portal issues it: three separate calls against one run.
	// That is the redundant-parse pattern §8.2's useSingleRun collapses, and the
	// surface #1665's unconfirmed timeout is reported against.
	opRunDetail = "run_detail_summary_events_attempts"
)

// measure builds a telemetry rollup over the generated instance and samples the
// read paths the portal exercises. It rebuilds the rollup from scratch, then
// attaches it to a read service exactly as the daemon does.
//
// samples is the number of times each read path is timed; the first is reported
// as cold and the rest as the warm distribution. See minSamplesForP999 for why
// the count matters to what may be claimed.
func measure(layout instance.Layout, gen GenerateResult, samples int, noFsync bool) (Measurement, error) {
	if samples < 2 {
		samples = 2 // one cold plus at least one warm, or there is no distribution
	}
	m := Measurement{
		Host:                 describeHost(os.Getenv("CI") != "", noFsync),
		Runs:                 gen.Runs,
		OrphanDirs:           gen.OrphanDirs,
		SchedulerEvents:      gen.SchedulerEvents,
		SchedulerJournalSize: gen.SchedulerJournalSize,
	}
	// RebuildAll over every per-gaggle run root, not Rebuild over one directory:
	// runs live in gaggles/<g>/runs, so the single-root form silently ingests
	// nothing and every subsequent read measures an empty index.
	roots, err := layout.RunDirs()
	if err != nil {
		return Measurement{}, fmt.Errorf("scale: enumerate run roots: %w", err)
	}
	m.RunEventsSize = treeSizeAcross(roots, "events.jsonl")
	m.SpansSize = treeSizeUnderAcross(roots, "spans")

	rebuildStart := time.Now()
	if err := rollup.RebuildAll(context.Background(), layout.TelemetryDB(), roots, layout.SchedulerDir()); err != nil {
		return Measurement{}, fmt.Errorf("scale: rebuild rollup: %w", err)
	}
	m.RollupRebuild = time.Since(rebuildStart)
	// The rebuild opens each published run's journal once; the corpus size is
	// therefore the open count, measured rather than assumed.
	m.RebuildOpens = uint64(gen.Runs)
	m.BlobPerOpenLatency = slowDiskDelay
	m.SimulatedBlobRebuild = m.RollupRebuild + simulatedOpenLatency(m.RebuildOpens, slowDiskDelay)

	if info, err := os.Stat(layout.TelemetryDB()); err == nil {
		m.TelemetryDBSize = info.Size()
	}

	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		return Measurement{}, fmt.Errorf("scale: open rollup: %w", err)
	}
	defer func() { _ = db.Close() }()

	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      layout,
		Definitions: instancefixture.Inventory(gen.Inventory),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		return Measurement{}, fmt.Errorf("scale: construct read service: %w", err)
	}
	ctx := context.Background()

	// Model the daemon, not a one-shot CLI: the daemon moves the active-run count
	// off the request path with a background sampler (#1741), and a harness that
	// omits it would measure a code path production does not take. Readers then
	// serve the last sample from memory.
	stopSampler := service.StartActiveRunSampler(time.Hour)
	defer func() { _ = stopSampler() }()
	// Wait for the first sample so the measurement reflects a warm daemon rather
	// than the "not sampled yet" state. That state is real and the portal renders
	// it, but it is a startup transient, not the steady state under test.
	if err := waitForActiveSample(ctx, service); err != nil {
		return Measurement{}, err
	}

	// A deep cursor is resolved once, outside the timed loop, so the deep-page
	// stat measures the page fetch rather than the walk that found the cursor.
	deepCursor, err := cursorAtDepth(ctx, service, 10)
	if err != nil {
		return Measurement{}, err
	}

	// A concrete run to measure the detail surfaces against, and a stage on it.
	// Resolved outside the loop for the same reason as the cursor.
	sampleRun, sampleStage, err := sampleRunAndStage(ctx, service)
	if err != nil {
		return Measurement{}, err
	}

	// The gaggle and workflow the inventory actually declares. Using a name the
	// definitions do not contain is the quiet way to measure nothing: the
	// surfaces return empty and look fast.
	firstGaggle := instancefixture.GaggleName(0)
	firstWorkflow := instancefixture.WorkflowName(0)

	ops := []struct {
		name string
		fn   func() error
	}{
		{opListRunsPage, func() error {
			_, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50})
			return err
		}},
		{opListRunsGaggle, func() error {
			_, err := service.ListRuns(ctx, readservice.RunListOptions{Gaggle: firstGaggle, Limit: 50})
			return err
		}},
		// A deep page is measured because keyset pagination is only bounded if
		// page N costs what page 1 costs; an offset-shaped plan degrades with
		// depth and a first-page-only measurement never sees it (§7.3).
		{opListRunsDeepPage, func() error {
			_, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50, Cursor: deepCursor})
			return err
		}},
		{opOverviewFanout, func() error {
			for _, phase := range dashboardPhases {
				if _, err := service.ListRuns(ctx, readservice.RunListOptions{Phase: phase, Limit: 50}); err != nil {
					return err
				}
			}
			return nil
		}},
		{opStatusFullScan, func() error {
			_, err := service.ListStatusRuns(ctx)
			return err
		}},

		// The four inventory surfaces §2.1 names. Each reaches activeRunCounts.
		{opInstance, func() error {
			_, err := service.Instance(ctx)
			return err
		}},
		{opGaggles, func() error {
			_, err := service.Gaggles(ctx, readservice.PageRequest{Limit: 50})
			return err
		}},
		{opWorkflows, func() error {
			_, err := service.Workflows(ctx, firstGaggle, readservice.PageRequest{Limit: 50})
			return err
		}},
		{opWorkflowDetail, func() error {
			_, err := service.Workflow(ctx, firstGaggle, firstWorkflow)
			return err
		}},
		// The Overview's own list path, and the fifth call site §2.1 adds. Issued
		// without a limit, exactly as the portal issues it (operationalData.ts).
		{opLatestPerWorkflow, func() error {
			_, err := service.ListRuns(ctx, readservice.RunListOptions{LatestPerWorkflow: true})
			return err
		}},

		// Run detail, all three calls, against one run — the pattern that
		// reparses the same bytes up to three times today.
		{opRunDetail, func() error {
			if _, err := service.GetRun(ctx, sampleRun); err != nil {
				return err
			}
			if _, err := service.RunEvents(ctx, sampleRun); err != nil {
				return err
			}
			if sampleStage == "" {
				return nil
			}
			_, err := service.StageAttempts(ctx, sampleRun, sampleStage)
			return err
		}},
	}

	// One instrumented pass per operation, before the timing loop, so the
	// counters describe a single invocation and the probe's atomic loads never
	// land inside a measured duration.
	m.Work = map[string]readprobe.Snapshot{}
	readprobe.Enable()
	for _, op := range ops {
		before := readprobe.Take()
		if err := op.fn(); err != nil {
			readprobe.Disable()
			return Measurement{}, fmt.Errorf("scale: %s (work pass): %w", op.name, err)
		}
		m.Work[op.name] = readprobe.Take().Sub(before)
	}
	readprobe.Disable()

	for _, op := range ops {
		// The full status scan is the deliberately-unindexed worst case; at 1×
		// it is seconds, so sampling it as often as an indexed page would make
		// the harness's own runtime the bottleneck.
		n := samples
		if op.name == opStatusFullScan && n > statusScanSamples {
			n = statusScanSamples
		}
		taken := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			start := time.Now()
			if err := op.fn(); err != nil {
				return Measurement{}, fmt.Errorf("scale: %s: %w", op.name, err)
			}
			taken = append(taken, time.Since(start))
		}
		m.Stats = append(m.Stats, summarize(op.name, taken))
	}

	return m, nil
}

// statusScanSamples caps repetitions of the unindexed full scan.
const statusScanSamples = 3

// cursorAtDepth walks `pages` pages and returns the cursor that follows them, so
// a deep page can be timed without the walk counting against it. An empty
// cursor means the corpus was too small to reach that depth, and the deep-page
// stat then simply re-measures the first page.
func cursorAtDepth(ctx context.Context, service *readservice.Local, pages int) (string, error) {
	options := readservice.RunListOptions{Limit: 50}
	for i := 0; i < pages; i++ {
		page, err := service.ListRuns(ctx, options)
		if err != nil {
			return "", fmt.Errorf("scale: seek to page %d: %w", i, err)
		}
		if page.NextCursor == "" {
			return options.Cursor, nil
		}
		options.Cursor = page.NextCursor
	}
	return options.Cursor, nil
}

// waitForActiveSample blocks until the background active-run sampler has
// published its first sample, so the measured steady state is a warm daemon.
func waitForActiveSample(ctx context.Context, service *readservice.Local) error {
	deadline := time.Now().Add(activeSampleWait)
	for {
		if _, err := service.Instance(ctx); err == nil {
			return nil
		} else if !errors.Is(err, readservice.ErrActiveCountsUnavailable) {
			return fmt.Errorf("scale: warm the active-run sampler: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("scale: active-run sampler produced no sample within %s", activeSampleWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// activeSampleWait bounds how long the harness waits for the first sample. At
// 10x the walk is tens of seconds, so this is generous rather than tight.
const activeSampleWait = 5 * time.Minute

// sampleRunAndStage returns a run id and one of its stage names, for measuring
// the run-detail surfaces against.
//
// It takes the *first* page's first run rather than a fixed id, so it works
// against any corpus size and against a corpus whose oldest runs have been
// pruned. An empty stage is returned rather than an error when the run has no
// stage attempts, because a corpus of one in-flight run is legitimate and the
// caller simply skips the attempts call.
func sampleRunAndStage(ctx context.Context, service *readservice.Local) (string, string, error) {
	page, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 1})
	if err != nil {
		return "", "", fmt.Errorf("scale: resolve sample run: %w", err)
	}
	if len(page.Runs) == 0 {
		return "", "", fmt.Errorf("scale: corpus has no readable runs to measure detail against")
	}
	runID := page.Runs[0].ID
	detail, err := service.GetRun(ctx, runID)
	if err != nil {
		return "", "", fmt.Errorf("scale: resolve sample run %s: %w", runID, err)
	}
	for _, stage := range detail.Stages {
		if stage != "" {
			return runID, stage, nil
		}
	}
	return runID, "", nil
}
