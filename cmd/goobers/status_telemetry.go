package main

import "github.com/goobers/goobers/internal/readservice"

func telemetryDestinationStatusLines(health *readservice.TelemetryExporterHealthStatus) string {
	return readservice.TelemetryDestinationStatusLines(health)
}
