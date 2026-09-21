package main

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/featureusage"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry"
)

// Feature windows are bounded absolute snapshots, never deltas to sum across
// heartbeats. A process restart clips the window at the new heartbeat boot.
const fleetUsageWindow = 5 * time.Minute
const fleetUsageGaggleLimit = 8

type fleetUsageSample func(context.Context, time.Time, []telemetry.DiagnosticRecord) []telemetry.DiagnosticRecord

// configured is evaluated on each sample so an accepted config reload changes
// configured feature labels without treating those labels as observed use.
func newFleetUsageSampler(root string, configured func() map[string]map[string]bool) fleetUsageSample {
	cursor := 0
	return func(ctx context.Context, now time.Time, heartbeats []telemetry.DiagnosticRecord) []telemetry.DiagnosticRecord {
		var current map[string]map[string]bool
		if configured != nil {
			current = configured()
		}
		records := []telemetry.DiagnosticRecord{}
		heartbeats = usageGaggles(heartbeats)
		if len(heartbeats) == 0 {
			return records
		}
		startCursor := cursor % len(heartbeats)
		cursor = (startCursor + fleetUsageGaggleLimit) % len(heartbeats)
		inspected := 0
		for index := range heartbeats {
			heartbeat := heartbeats[(startCursor+index)%len(heartbeats)]
			if heartbeat.Name != "goobers.fleet.heartbeat" {
				continue
			}
			gaggle, _ := heartbeat.Attributes["gaggleId"].(string)
			// No deployment-level configured feature set is implied by gaggle config.
			if gaggle == "" {
				continue
			}
			start := now.Add(-fleetUsageWindow)
			boot, _ := heartbeat.Attributes["bootStartedAt"].(string)
			bootTime, err := time.Parse(time.RFC3339Nano, boot)
			if err != nil || bootTime.After(now) {
				continue
			}
			if start.Before(bootTime) {
				start = bootTime
			}
			counts := map[string]featureusage.Count{}
			if inspected < fleetUsageGaggleLimit && ctx.Err() == nil && gaggle == filepath.Base(gaggle) && gaggle != "." && gaggle != ".." && !strings.ContainsAny(gaggle, "/\\") {
				counts = featureusage.Scan(ctx, instance.NewLayout(root).ForGaggle(gaggle).RunsDir(), start, now)
				inspected++
			}
			for _, id := range featureusage.IDs() {
				configuredValue, known := current[gaggle][id]
				if !known {
					continue
				}
				attrs := usageIdentity(heartbeat.Attributes)
				attrs["featureId"] = id
				attrs["configured"] = configuredValue
				attrs["windowStart"] = start.Format(time.RFC3339Nano)
				attrs["windowEnd"] = now.Format(time.RFC3339Nano)
				attrs["observedAt"] = now.Format(time.RFC3339Nano)
				count := counts[id]
				attrs["windowCoverage"] = "partial"
				if count.Complete && heartbeat.Attributes["windowCoverage"] == "complete" {
					attrs["windowCoverage"] = "complete"
					attrs["count"] = count.Value
				} else if count.Value > 0 {
					attrs["count"] = count.Value
				}
				records = append(records, telemetry.DiagnosticRecord{Time: now, Name: "goobers.feature.usage", Attributes: attrs})
			}
		}
		return records
	}
}
func usageIdentity(source map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{"schemaVersion", "organization", "environment", "deploymentId", "instanceId", "gaggleId", "ownerRef", "component", "bootId", "bootStartedAt", "sequence"} {
		if value, ok := source[key]; ok {
			result[key] = value
		}
	}
	return result
}

func usageGaggles(records []telemetry.DiagnosticRecord) []telemetry.DiagnosticRecord {
	result := make([]telemetry.DiagnosticRecord, 0, 100)
	for _, record := range records {
		gaggle, _ := record.Attributes["gaggleId"].(string)
		if record.Name == "goobers.fleet.heartbeat" && gaggle != "" {
			result = append(result, record)
		}
		if len(result) == 100 {
			break
		}
	}
	return result
}
