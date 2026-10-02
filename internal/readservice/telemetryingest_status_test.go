package readservice

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// TestSchedulerStatusReportsTelemetryIngestHealth pins #5562's surfacing half:
// scheduler status carries telemetry.db's ingest health, so a skipped corrupt
// record (or a failing ingest) is visible without reading the scheduler log.
func TestSchedulerStatusReportsTelemetryIngestHealth(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service, err := NewLocal(LocalSources{
		Layout: layout, Definitions: testDefinitions(), Telemetry: db,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	healthy, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if healthy.TelemetryIngest != nil {
		t.Fatalf("TelemetryIngest = %+v before any trouble, want nil", healthy.TelemetryIngest)
	}

	spansDir := filepath.Join(layout.SchedulerDir(), "spans")
	if err := os.MkdirAll(spansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spansDir, "spans.jsonl"), []byte("{\"spanId\": \"x\" broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestSchedulerLog(context.Background(), layout.SchedulerDir()); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}
	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.TelemetryIngest == nil || status.TelemetryIngest.SkippedRecords != 1 || status.TelemetryIngest.FailingSince != nil {
		t.Fatalf("TelemetryIngest = %+v, want one skipped record and no failure", status.TelemetryIngest)
	}
}
