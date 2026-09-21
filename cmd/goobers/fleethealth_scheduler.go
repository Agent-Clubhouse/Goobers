package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/fleetstate"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
)

type fleetSchedulerReader interface {
	SchedulerStatus(context.Context) (readservice.SchedulerStatus, error)
}

func observeFleetScheduler(observation *fleetstate.Observation, status *readservice.SchedulerStatus, gaggle string, now time.Time) {
	if status == nil {
		return
	}
	if status.ProviderQuotaResumeAt != nil && status.ProviderQuotaResumeAt.After(now) {
		observation.ProviderThrottled = true
		observation.BackoffUntil = *status.ProviderQuotaResumeAt
	}
	if storage := status.StorageHealth; storage != nil && !storage.MeasuredAt.IsZero() && !storage.MeasuredAt.After(now) && now.Sub(storage.MeasuredAt) <= time.Minute {
		observation.StorageFailure = storage.Tier == "admission-stopped"
	}
	if maintenance := status.Maintenance; maintenance != nil {
		observation.CleanupFailure = maintenance.State == "running" && maintenance.Failures > 0
	}
	for _, occupancy := range status.RefillOccupancy {
		if occupancy.Gaggle != gaggle || !occupancy.AdmissionBlocked {
			continue
		}
		switch occupancy.BlockingCondition {
		case localscheduler.ReasonMaxParallel, localscheduler.ReasonInstanceMaxParallel, localscheduler.ReasonMemoryPressure:
			observation.AdmissionSaturated = true
		}
	}
}
