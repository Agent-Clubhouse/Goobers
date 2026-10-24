package main

import (
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
)

func newDatabaseServices(pools *cloudpg.Pools, store *acceptance.Store, compatibility error) databaseServices {
	return databaseServices{
		Pools: pools, Acceptances: store, SchemaCompatibility: compatibility,
		BindControllerFence: store.BindFence,
		RecoverStarts:       store.RecoverStarts,
		StartStatusPage:     store.StartStatusPage,
		NewStarter: func(c client.Client, queue string) engine.Starter {
			return startfence.New(c, queue, acceptance.NewStartAdmission(store), nil)
		},
	}
}
