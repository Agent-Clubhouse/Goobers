package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAgedSchedulerEvent writes one scheduler journal record stamped long
// before any plausible retention window, so compaction treats it as aged.
func writeAgedSchedulerEvent(t *testing.T, root string) string {
	t.Helper()
	schedulerDir := filepath.Join(root, "scheduler")
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"schema":"goobers.dev/journal/event/v1","seq":1,"time":"2020-01-01T00:00:00Z","type":"trigger.fired","workflow":"a"}` + "\n"
	path := filepath.Join(schedulerDir, "events.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTelemetryCompactDryRunLeavesJournal(t *testing.T) {
	root := initDemo(t)
	path := writeAgedSchedulerEvent(t, root)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "telemetry", "compact", "--dry-run", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "would compact scheduler journal: 1 record") {
		t.Fatalf("stdout = %q, want a dry-run drop report", stdout)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry-run modified the journal")
	}
}

func TestTelemetryCompactDropsAgedJournalRecords(t *testing.T) {
	root := initDemo(t)
	path := writeAgedSchedulerEvent(t, root)

	code, stdout, stderr := runArgs(t, "telemetry", "compact", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "compacted scheduler journal: 1 record") {
		t.Fatalf("stdout = %q, want a compaction report", stdout)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "2020-01-01") {
		t.Fatalf("aged record still present after compaction: %s", data)
	}
}
