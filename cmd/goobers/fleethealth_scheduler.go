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

// Cleanup evidence belongs to the deployment. A failed sweep does not prove
// that a particular gaggle is blocked or that unrelated work cannot progress.
func observeFleetCleanup(attrs map[string]any, status *readservice.SchedulerStatus, now time.Time) {
	if status == nil || status.Maintenance == nil {
		return
	}
	// Failed counts reset at the next pass; running is not a failed completion.
	// Use completion time, not progress time, to avoid refreshing old failures.
	maintenance := status.Maintenance
	at := maintenance.LastCompletedAt
	if at == nil || at.IsZero() || at.After(now) || now.Sub(*at) > time.Minute || maintenance.State != "failed" || maintenance.LastResult != "failed" || maintenance.Failures <= 0 {
		return
	}
	attrs["state"], attrs["reasonCode"] = "unknown", "cleanup_failure"
}
