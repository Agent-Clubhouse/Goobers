package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/projectorpg"
)

// projectorService returns the read-model projector's lifetime: it holds its
// own "projector" lease (separate from the controller's, so projection can never
// fence admission) and, while holding it, keeps the read model current with the
// journal. It stays a standby on other replicas.
func projectorService(db databaseServices, services controlPlanes, report func(error)) (func(context.Context) error, error) {
	if db.Pools == nil || db.InstanceID == "" || services.journalReader == nil {
		return nil, errors.New("control projector requires the controller database and the journal plane")
	}
	manager, err := lease.New(lease.Config{Pool: db.Pools.Direct, InstanceID: db.InstanceID, Name: projectorpg.LeaseName, Holder: environmentOr("GOOBERS_REPLICA_ID", defaultMigrationHolder())})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		return projectorpg.Serve(ctx, projectorpg.ServeConfig{
			Manager: manager, Pool: db.Pools.Transaction, Journal: services.journalReader,
			InstanceID: db.InstanceID, Wake: services.projectorWake, Report: report,
		})
	}, nil
}
