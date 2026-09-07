package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

func TestTelemetryMergesReportsRealJournalConfirmation(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	id := strings.Repeat("a", 32)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: "merge-report", Workflow: "landing", Gaggle: "web", InstanceID: id}, nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatal(err)
	}
	confirmation := &providers.MergeConfirmation{RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: "9", MergeSHA: "commit"}
	if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "9"}, Runner: providers.MutationRunnerFields("merge", confirmation)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.TelemetryDB()), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.IngestRun(context.Background(), run.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "telemetry", "merges", "--json", "--since=2026-09-01T00:00:00Z", "--until=2026-09-02T00:00:00Z", layout.Root)
	if code != 0 {
		t.Fatalf("report failed: %d %s", code, stderr)
	}
	var report rollup.MergeReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Merges) != 1 || report.Merges[0].InstanceID != id || report.Merges[0].PullID != "9" || len(report.Daily) != 1 || report.Daily[0].Count != 1 {
		t.Fatalf("CLI lost provenance: %+v", report)
	}
}
