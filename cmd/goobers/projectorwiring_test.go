package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readmodel/projector"
	"github.com/goobers/goobers/internal/readmodel/repair"
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

	stop, _, _ := startProjector(context.Background(), store, watermarks, layout, nil)
	defer stop()

	if repairWriter == store {
		t.Fatal("production wiring gave repair the raw store as its writer")
	}
	if _, ok := repairWriter.(*projector.Projector); !ok {
		t.Fatalf("repair writer = %T, want *projector.Projector", repairWriter)
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

	stop, _, _ := startProjector(context.Background(), store, watermarks, layout, &instance.Config{})
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
	stop, _, _ := startProjector(context.Background(), store, watermarks, layout, cfg)
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

	stop, _, stats := startProjector(context.Background(), store, watermarks, layout, &instance.Config{})
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
