package main

import (
	"context"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
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
	m.RunEventsSize = treeSize(layout.RunsDir(), "events.jsonl")
	m.SpansSize = treeSizeUnder(layout.RunsDir(), "spans")

	rebuildStart := time.Now()
	if err := rollup.Rebuild(layout.TelemetryDB(), layout.RunsDir(), layout.SchedulerDir()); err != nil {
		return Measurement{}, fmt.Errorf("scale: rebuild rollup: %w", err)
	}
	m.RollupRebuild = time.Since(rebuildStart)

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
		Definitions: minimalDefinitions(),
		Telemetry:   db,
	}, func() bool { return true })
	if err != nil {
		return Measurement{}, fmt.Errorf("scale: construct read service: %w", err)
	}
	ctx := context.Background()

	// A deep cursor is resolved once, outside the timed loop, so the deep-page
	// stat measures the page fetch rather than the walk that found the cursor.
	deepCursor, err := cursorAtDepth(ctx, service, 10)
	if err != nil {
		return Measurement{}, err
	}

	ops := []struct {
		name string
		fn   func() error
	}{
		{opListRunsPage, func() error {
			_, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50})
			return err
		}},
		{opListRunsGaggle, func() error {
			_, err := service.ListRuns(ctx, readservice.RunListOptions{Gaggle: gaggles[0], Limit: 50})
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
	}

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

// minimalDefinitions builds the smallest ConfigSet readservice.NewLocal accepts.
//
// NOTE: an empty inventory does NOT spare the harness the active-run scan —
// Local.Instance calls activeRunCounts unconditionally and it is keyed on run
// directories, not on configured workflows (design §2.5). The reason this
// harness has never measured that scan is simply that it never calls
// Instance/Gaggles/Workflows. A parameterizable inventory and coverage of those
// surfaces is the next slice of #1913.
func minimalDefinitions() *instance.ConfigSet {
	return &instance.ConfigSet{Manifest: &apiv1.Manifest{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			workflow.PreviewFeaturesAnnotation: "true",
		}},
		Spec: apiv1.ManifestSpec{
			Instance: apiv1.InstanceRef{Name: "scale-harness", Environment: apiv1.EnvironmentDev},
		},
	}}
}
