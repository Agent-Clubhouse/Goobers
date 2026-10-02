package main

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

func TestTelemetryIngestStatusLine(t *testing.T) {
	since := time.Date(2026, 9, 12, 17, 25, 0, 0, time.UTC)
	skipAt := since.Add(time.Hour)
	now := skipAt.Add(time.Hour)
	if got := telemetryIngestStatusLine(readservice.SchedulerStatus{}, now); got != "" {
		t.Fatalf("healthy ingest line = %q, want empty", got)
	}
	ingest := &readservice.TelemetryIngestStatus{
		FailingSince: &since, LastFailure: "rollup: event schema unsupported",
		SkippedRecords: 3, LastSkipAt: &skipAt, LastSkip: "corrupt record at byte offset 42",
	}
	got := telemetryIngestStatusLine(readservice.SchedulerStatus{TelemetryIngest: ingest}, now)
	for _, want := range []string{
		"Warning: scheduler telemetry ingest failing since 2026-09-12T17:25:00Z: rollup: event schema unsupported\n",
		"skipped 3 corrupt record(s); last at 2026-09-12T18:25:00Z: corrupt record at byte offset 42\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ingest line = %q, want it to contain %q", got, want)
		}
	}

	// A skip long past stops being reported; a current failure never does.
	later := skipAt.Add(telemetryIngestSkipWindow + time.Minute)
	got = telemetryIngestStatusLine(readservice.SchedulerStatus{TelemetryIngest: ingest}, later)
	if strings.Contains(got, "skipped") || !strings.Contains(got, "failing since") {
		t.Fatalf("ingest line after the skip window = %q, want only the failure warning", got)
	}
}
