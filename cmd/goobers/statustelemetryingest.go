package main

import (
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

// telemetryIngestSkipWindow is how long after its newest skip the status line
// keeps reporting corrupt scheduler-log records. The count is lifetime, so
// without a window one skipped line would print forever.
const telemetryIngestSkipWindow = 7 * 24 * time.Hour

// telemetryIngestStatusLine surfaces scheduler-telemetry ingest trouble
// (#5562). A failing ingest used to report only into the scheduler log it
// could not read, so scheduler telemetry went silently stale for days; this
// line reads the health telemetry.db records instead.
func telemetryIngestStatusLine(status readservice.SchedulerStatus, now time.Time) string {
	ingest := status.TelemetryIngest
	if ingest == nil {
		return ""
	}
	var line string
	if ingest.FailingSince != nil {
		line = fmt.Sprintf("Warning: scheduler telemetry ingest failing since %s: %s\n",
			ingest.FailingSince.UTC().Format(time.RFC3339), ingest.LastFailure)
	}
	if ingest.SkippedRecords > 0 && ingest.LastSkipAt != nil && now.Sub(*ingest.LastSkipAt) < telemetryIngestSkipWindow {
		line += fmt.Sprintf("Scheduler telemetry ingest skipped %d corrupt record(s); last at %s: %s\n",
			ingest.SkippedRecords, ingest.LastSkipAt.UTC().Format(time.RFC3339), ingest.LastSkip)
	}
	return line
}
