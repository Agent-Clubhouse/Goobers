package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/cloud/cloudreader"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/readmodelpg"
	"github.com/goobers/goobers/internal/readservice"
)

// constructReader serves the control plane's reads (#7818 R3): a rootless read
// service over the journal plane and the PostgreSQL read model, rebuilt on
// every applied configuration generation. journal.* is controller-only, so it
// is a controller seam composed after Configuration and Journal.
func constructReader(ctx context.Context, db databaseServices, _ controlAuth, _ *lease.Manager, owner func() *lease.Handle, p *controlPlanes) error {
	applied, ok := p.Configuration.(cloudreader.AppliedConfiguration)
	if !ok {
		return errors.New("control reads require the applied configuration (#7860)")
	}
	if p.journalReader == nil || db.Pools == nil {
		return errors.New("control reads require the journal plane and the controller database")
	}
	model, err := readmodelpg.NewActiveReader(ctx, db.Pools.Transaction)
	if err != nil {
		return err
	}
	health := p.projectorHealth
	reader, err := cloudreader.New(cloudreader.Config{
		Journal: p.journalReader, ReadModel: model, Applied: applied,
		ProjectionHealth: func() readservice.ProjectionHealth {
			snapshot := health.Snapshot()
			return readservice.ProjectionHealth{ApplyFailures: snapshot.ApplyFailures, LastDrainAt: snapshot.LastDrainAt}
		},
		// Reads are ready on the replica holding the controller lease.
		Ready: func() bool { return owner() != nil },
	})
	if err != nil {
		return err
	}
	p.Reader = reader
	return nil
}
