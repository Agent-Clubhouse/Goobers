package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry"
)

type fleetFeatureConfigurationReader interface {
	DiagnosticFeatureConfiguration() map[string]map[string]bool
}

func combinedFleetSampler(root string, setup *schedulerSetup, reader fleetHealthReader, health fleetHealthSample) fleetHealthSample {
	usage := newFleetUsageSampler(root, currentFeatureConfiguration(setup, reader), setup.Config.Telemetry.Diagnostics.HeartbeatPeriod())
	return func(ctx context.Context, now time.Time) []telemetry.DiagnosticRecord {
		records := health(ctx, now)
		return append(records, usage(ctx, now, records)...)
	}
}
func currentFeatureConfiguration(setup *schedulerSetup, reader fleetHealthReader) func() map[string]map[string]bool {
	return func() map[string]map[string]bool {
		source, ok := reader.(fleetFeatureConfigurationReader)
		if !ok {
			return nil
		}
		configured := source.DiagnosticFeatureConfiguration()
		if setup == nil || setup.Interventions == nil {
			return configured
		}
		overlayFeatureDrivers(configured, setup.Interventions.Snapshot().featureDrivers)
		return configured
	}
}

func overlayFeatureDrivers(configured map[string]map[string]bool, drivers map[localscheduler.WorkflowIdentity]string) {
	unknown := map[string]bool{}
	for identity, id := range drivers {
		features := configured[identity.Gaggle]
		if features == nil {
			continue
		}
		if _, known := features["runner.local"]; !known {
			features["runner.local"] = false
			features["runner.engine"] = false
		}
		if id == "" {
			unknown[identity.Gaggle] = true
		} else {
			features[id] = true
		}
	}
	for gaggle := range unknown {
		for _, id := range []string{"runner.local", "runner.engine"} {
			if !configured[gaggle][id] {
				delete(configured[gaggle], id)
			}
		}
	}
}
