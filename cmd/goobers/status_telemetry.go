package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/readservice"
)

func telemetryDestinationStatusLines(health *readservice.TelemetryExporterHealthStatus) string {
	if health == nil {
		return ""
	}
	names := make([]string, 0, len(health.Destinations))
	for name := range health.Destinations {
		names = append(names, name)
	}
	sort.Strings(names)
	var text strings.Builder
	for _, name := range names {
		d := health.Destinations[name]
		fmt.Fprintf(&text, "Telemetry destination %s: kind=%s trace=%s metric=%s", name, d.Mode, d.Trace.State, d.Metric.State)
		if d.UnavailableReason != "" {
			fmt.Fprintf(&text, " unavailable=%s", d.UnavailableReason)
		}
		if d.Journal != nil {
			fmt.Fprintf(&text, " journal-dropped=%d journal-failures=%d", d.Journal.Dropped, d.Journal.Failures)
		}
		if d.Diagnostics != nil {
			fmt.Fprintf(&text, " diagnostic-dropped=%d diagnostic-failures=%d", d.Diagnostics.Dropped, d.Diagnostics.Failures)
		}
		if d.Replay != nil {
			fmt.Fprintf(&text, " replay-pending=%d replay-failed=%t", d.Replay.PendingRecords, d.Replay.ActiveFailure)
		}
		text.WriteByte('\n')
	}
	return text.String()
}
