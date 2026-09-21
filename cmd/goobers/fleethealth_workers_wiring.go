package main

import "github.com/goobers/goobers/internal/localscheduler"

func newFleetWorkerHealthObserver(setup *schedulerSetup, engine *daemonEngineClient) *fleetWorkerHealthObserver {
	observer := &fleetWorkerHealthObserver{client: engine.Temporal()}
	if setup == nil || setup.Config == nil {
		return observer
	}
	observer.queue = setup.Config.EffectiveEngineConfig().TaskQueue
	observer.drivers = func() map[localscheduler.WorkflowIdentity]string {
		return setup.Interventions.Snapshot().featureDrivers
	}
	return observer
}
