package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/fleetstate"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
)

func TestFleetSchedulerFactsAndClearing(t *testing.T) {
	now := time.Now().UTC()
	resume := now.Add(time.Minute)
	cases := []struct {
		name   string
		status readservice.SchedulerStatus
		reason string
	}{
		{"quota", readservice.SchedulerStatus{ProviderQuotaResumeAt: &resume}, "provider_throttled"},
		{"storage", readservice.SchedulerStatus{StorageHealth: &readservice.StorageHealthStatus{Tier: "admission-stopped", MeasuredAt: now}}, "storage_failure"},
		{"cleanup", readservice.SchedulerStatus{Maintenance: &readservice.MaintenanceStatus{State: "running", Failures: 1}}, "cleanup_failure"},
		{"admission", readservice.SchedulerStatus{RefillOccupancy: []readservice.RefillOccupancyStatus{{Gaggle: "alpha", AdmissionBlocked: true, BlockingCondition: localscheduler.ReasonInstanceMaxParallel}}}, "admission_saturated"},
	}
	zero := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observation := fleetstate.Observation{ObservedAt: now, Complete: true, EligibleCount: &zero, InflightCount: &zero}
			observeFleetScheduler(&observation, &tc.status, "alpha", now)
			if got := fleetstate.Classify(observation, now, time.Minute, time.Minute); got.ReasonCode != tc.reason {
				t.Fatalf("got %+v", got)
			}
			// Every pulse starts from fresh facts, so old failures do not latch forever.
			recovered := fleetstate.Observation{ObservedAt: now, Complete: true, EligibleCount: &zero, InflightCount: &zero}
			observeFleetScheduler(&recovered, &readservice.SchedulerStatus{}, "alpha", now)
			if got := fleetstate.Classify(recovered, now, time.Minute, time.Minute); got.State != "idle" {
				t.Fatalf("recovery did not clear condition: %+v", got)
			}
		})
	}
}
func TestFleetSchedulerRejectsStaleStorageAndOtherGaggle(t *testing.T) {
	now := time.Now().UTC()
	observation := fleetstate.Observation{}
	status := readservice.SchedulerStatus{StorageHealth: &readservice.StorageHealthStatus{Tier: "admission-stopped", MeasuredAt: now.Add(-time.Hour)}, RefillOccupancy: []readservice.RefillOccupancyStatus{{Gaggle: "other", AdmissionBlocked: true, BlockingCondition: localscheduler.ReasonMaxParallel}}}
	observeFleetScheduler(&observation, &status, "alpha", now)
	if observation.StorageFailure || observation.AdmissionSaturated {
		t.Fatal("stale or differently scoped evidence attributed to gaggle")
	}
}
