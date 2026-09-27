package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readmodel/projector"
	"github.com/goobers/goobers/internal/readmodel/repair"
	"github.com/goobers/goobers/internal/readservice"
)

func TestStartProjectorRoutesRepairWritesThroughCommitLoop(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	watermarks, err := intake.Open(layout.IntakeDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watermarks.Close() }()

	var repairWriter repair.Writer
	original := newRepairSweeper
	newRepairSweeper = func(
		repairStore repair.Store,
		writer repair.Writer,
		watermarks repair.Watermarks,
		options repair.Options,
	) *repair.Sweeper {
		repairWriter = writer
		return repair.New(repairStore, writer, watermarks, options)
	}
	t.Cleanup(func() { newRepairSweeper = original })

	stop, _, _, _ := startProjector(context.Background(), store, watermarks, layout, nil)
	defer stop()

	if repairWriter == store {
		t.Fatal("production wiring gave repair the raw store as its writer")
	}
	if _, ok := repairWriter.(*projector.Projector); !ok {
		t.Fatalf("repair writer = %T, want *projector.Projector", repairWriter)
	}
}

func TestStartProjectorRepairsStaleRunningRowsAcrossGaggles(t *testing.T) {
	ctx := context.Background()
	root := initDemo(t)
	layout := instance.NewLayout(root)
	if err := layout.EnsureGaggleRuntime("beta"); err != nil {
		t.Fatal(err)
	}
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	watermarks, err := intake.Open(layout.IntakeDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watermarks.Close() }()

	const runID = "00000000000000000000000000005308"
	writer, err := journal.Create(layout.ForGaggle("beta").RunsDir(), journal.RunIdentity{
		RunID: runID, Gaggle: "beta", Workflow: "merge-review", WorkflowVersion: 1,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(journal.Event{
		Type: journal.EventStageStarted, Stage: "review", Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRun(ctx, readmodel.Projection{Run: readmodel.RunRow{
		RunID: runID, Gaggle: "beta", Workflow: "merge-review",
		Phase: journal.PhaseRunning, StartedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
	run, _, err := journal.Recover(filepath.Join(layout.ForGaggle("beta").RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	stop, _, _, restartComplete := startProjector(ctx, store, watermarks, layout, nil)
	defer stop()
	if !restartComplete {
		t.Fatal("projector startup reconciliation did not complete")
	}
	row, ok, err := store.GetRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("reconciled beta run: found=%v err=%v", ok, err)
	}
	if row.Phase != journal.PhaseCompleted || !row.Terminal {
		t.Fatalf("beta run after startup = phase %q terminal=%v, want completed terminal", row.Phase, row.Terminal)
	}
}

func TestStartProjectorUsesDefaultRetentionWindowWhenUnset(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	watermarks, err := intake.Open(layout.IntakeDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watermarks.Close() }()

	var captured readmodel.RetentionWindow
	original := newRetentionLoop
	newRetentionLoop = func(
		s *readmodel.Store,
		w readmodel.RetentionWriter,
		window readmodel.RetentionWindow,
		options readmodel.RetentionOptions,
	) *readmodel.RetentionLoop {
		captured = window
		return readmodel.NewRetentionLoop(s, w, window, options)
	}
	t.Cleanup(func() { newRetentionLoop = original })

	stop, _, _, _ := startProjector(context.Background(), store, watermarks, layout, &instance.Config{})
	defer stop()
	if !captured.Bounded() || captured.Days() != instance.DefaultProjectionFullFidelityDays {
		t.Fatalf("retention window = %s, want %dd default", captured, instance.DefaultProjectionFullFidelityDays)
	}
}

func TestStartProjectorAllowsExplicitOptOutRetentionWindow(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	watermarks, err := intake.Open(layout.IntakeDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watermarks.Close() }()

	var captured readmodel.RetentionWindow
	original := newRetentionLoop
	newRetentionLoop = func(
		s *readmodel.Store,
		w readmodel.RetentionWriter,
		window readmodel.RetentionWindow,
		options readmodel.RetentionOptions,
	) *readmodel.RetentionLoop {
		captured = window
		return readmodel.NewRetentionLoop(s, w, window, options)
	}
	t.Cleanup(func() { newRetentionLoop = original })

	cfg := &instance.Config{}
	cfg.Retention.ProjectionFullFidelityDays = 0
	if err := cfg.Retention.UnmarshalJSON([]byte(`{"projectionFullFidelityDays":0}`)); err != nil {
		t.Fatalf("mark retention field configured: %v", err)
	}
	stop, _, _, _ := startProjector(context.Background(), store, watermarks, layout, cfg)
	defer stop()
	if captured.Bounded() || captured.Days() != 0 {
		t.Fatalf("retention window = %s, want unbounded opt-out", captured)
	}
}

// TestStartProjectorExposesStats pins that the daemon can see the projector's
// counters (#2843). Without this accessor the read service has no way to learn
// about a run the projector failed to apply, and the readState envelope reports
// a clean bill of health over a projection with a known gap.
func TestStartProjectorExposesStats(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	watermarks, err := intake.Open(layout.IntakeDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watermarks.Close() }()

	stop, _, stats, _ := startProjector(context.Background(), store, watermarks, layout, &instance.Config{})
	defer stop()

	if stats == nil {
		t.Fatal("startProjector reported no stats accessor")
	}
	// The restart pass has already completed a drain by the time this returns,
	// so the freshness surface has a real timestamp to report rather than the
	// zero time it must treat as "unknown".
	if stats().LastDrainAt.IsZero() {
		t.Error("lastDrainAt is zero after the restart pass; projection lag would " +
			"read as unknown on a projector that has in fact drained")
	}
}

// TestAttachFreshnessSignalsReachesTheEnvelope guards the daemon-path wiring
// itself (#2843).
//
// The bug was not a missing capability: AttachIntakeDepth existed and was
// exercised by tests, but nothing on the real startup path called it, so
// pendingIntake was permanently zero in production. Testing the accessor alone
// would leave exactly that hole open, so this asserts the whole chain — setup
// counters through the helper runUpContextWithForce calls, out to a served
// response's readState.
func TestAttachFreshnessSignalsReachesTheEnvelope(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	watermarks, err := intake.Open(layout.IntakeDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watermarks.Close() }()

	definitions, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		t.Fatalf("load fixture config: %v (report: %+v)", err, report)
	}
	reads, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      layout,
		ReadModel:   store,
		Definitions: definitions,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	attachFreshnessSignals(reads, &schedulerSetup{
		Watermarks: watermarks,
		ProjectorStats: func() projector.Stats {
			// A lifetime failure that has since been repaired plus one still
			// open: only the open one is a gap the operator can act on.
			return projector.Stats{ProjectFailures: 4, UnresolvedRuns: 1, LastDrainAt: time.Now()}
		},
	})

	list, err := reads.ListRuns(context.Background(), readservice.RunListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if list.ReadState == nil {
		t.Fatal("the response carries no readState; the freshness envelope is missing entirely")
	}
	if list.ReadState.Completeness != readmodel.CompletenessPartial {
		t.Errorf("completeness = %q, want %q: an unprojected run is missing from this list",
			list.ReadState.Completeness, readmodel.CompletenessPartial)
	}
	if !slices.Contains(list.ReadState.Degraded, readmodel.DegradedProjectFailure) {
		t.Errorf("degraded = %v, want it to contain %q",
			list.ReadState.Degraded, readmodel.DegradedProjectFailure)
	}
	for _, missing := range list.ReadState.Missing {
		// The lifetime counter must not leak into the operator-facing number:
		// reporting 4 when 3 have been repaired overstates a resolved gap.
		if strings.Contains(missing.Reason, "4 run(s)") {
			t.Errorf("missing reason %q reports the cumulative failure count rather than "+
				"the open gap", missing.Reason)
		}
	}
}

// TestAttachFreshnessSignalsToleratesAProjectorlessSetup pins that the daemon
// still starts when the intake store could not be opened: the envelope reports
// fewer signals, it does not fail.
func TestAttachFreshnessSignalsToleratesAProjectorlessSetup(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	definitions, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		t.Fatalf("load fixture config: %v (report: %+v)", err, report)
	}
	reads, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      layout,
		Definitions: definitions,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	attachFreshnessSignals(reads, &schedulerSetup{})
	attachFreshnessSignals(nil, nil)
	if _, err := reads.ListRuns(context.Background(), readservice.RunListOptions{Limit: 1}); err != nil {
		t.Errorf("listing runs after attaching nothing: %v", err)
	}
}
