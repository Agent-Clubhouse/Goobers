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

	stop := startProjector(context.Background(), store, watermarks, layout, nil)
	defer stop()

	if repairWriter == store {
		t.Fatal("production wiring gave repair the raw store as its writer")
	}
	if _, ok := repairWriter.(*projector.Projector); !ok {
		t.Fatalf("repair writer = %T, want *projector.Projector", repairWriter)
	}
}
