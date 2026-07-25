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
// the dashboard depends on (epic #1410). Durations are wall time for a single
// operation; sizes are on-disk bytes after ingest. These are the numbers the
// harness reports at 1×/10×/100× and the regression guard bounds.
type Measurement struct {
	Runs                 int
	SchedulerEvents      int
	SchedulerJournalSize int64
	TelemetryDBSize      int64

	// RollupRebuild is the cost of rollup.Rebuild — the full re-ingest of every
	// run journal plus the scheduler log into telemetry.db (the `--rebuild` and
	// cold-start path).
	RollupRebuild time.Duration
	// ListRunsFirstPage is the latency of one indexed ListRuns page (the DASH-18
	// indexed read the dashboard run list issues), including the first
	// reconcile scan.
	ListRunsFirstPage time.Duration
	// ListRunsWarmPage is a second indexed ListRuns page within the reconcile
	// window — the steady-state latency once the index is reconciled.
	ListRunsWarmPage time.Duration
	// OverviewFanout is the cost of the Overview's per-phase ListRuns fan-out,
	// the read pattern that regressed under load (#1367): one page request per
	// terminal phase, as the dashboard Overview issues them.
	OverviewFanout time.Duration
	// StatusScan is the full journal-scan ListStatusRuns cost — the unindexed
	// fallback and the worst case the index is meant to avoid.
	StatusScan time.Duration
}

// dashboardPhases mirror the phases the Overview fans out over, one ListRuns
// each — the read pattern OverviewFanout times.
var dashboardPhases = []journal.RunPhase{
	journal.PhaseRunning,
	journal.PhaseCompleted,
	journal.PhaseFailed,
	journal.PhaseEscalated,
	journal.PhaseAborted,
}

// measure builds a telemetry rollup over the generated instance and times the
// read paths the dashboard exercises. It rebuilds the rollup from scratch, then
// attaches it to a read service exactly as the daemon does and times an indexed
// page, a warm page, the Overview fan-out, and the full status scan.
func measure(layout instance.Layout, runs, schedulerEvents int, schedulerSize int64) (Measurement, error) {
	m := Measurement{Runs: runs, SchedulerEvents: schedulerEvents, SchedulerJournalSize: schedulerSize}

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

	// First page pays for the initial reconcile scan; the warm page within the
	// reconcile window reflects steady-state indexed latency.
	first := time.Now()
	if _, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50}); err != nil {
		return Measurement{}, fmt.Errorf("scale: list runs (first page): %w", err)
	}
	m.ListRunsFirstPage = time.Since(first)

	warm := time.Now()
	if _, err := service.ListRuns(ctx, readservice.RunListOptions{Limit: 50}); err != nil {
		return Measurement{}, fmt.Errorf("scale: list runs (warm page): %w", err)
	}
	m.ListRunsWarmPage = time.Since(warm)

	fanout := time.Now()
	for _, phase := range dashboardPhases {
		if _, err := service.ListRuns(ctx, readservice.RunListOptions{Phase: phase, Limit: 50}); err != nil {
			return Measurement{}, fmt.Errorf("scale: overview fan-out phase %q: %w", phase, err)
		}
	}
	m.OverviewFanout = time.Since(fanout)

	scan := time.Now()
	if _, err := service.ListStatusRuns(ctx); err != nil {
		return Measurement{}, fmt.Errorf("scale: status scan: %w", err)
	}
	m.StatusScan = time.Since(scan)

	return m, nil
}

// minimalDefinitions builds the smallest ConfigSet readservice.NewLocal accepts:
// a manifest with an instance ref and preview features on. The harness measures
// run read paths, not inventory, so no gaggles/workflows are needed.
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
