package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// writeAgedSchedulerEvent writes one scheduler journal record stamped long
// before any plausible retention window, so compaction treats it as aged.
func writeAgedSchedulerEvent(t *testing.T, root string) string {
	t.Helper()
	schedulerDir := filepath.Join(root, "scheduler")
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"schema":"goobers.dev/journal/event/v1","seq":1,"time":"2020-01-01T00:00:00Z","type":"tick.skipped","workflow":"a"}` + "\n"
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

	// #2265: compaction advances to a new generation rather than rewriting
	// path in place — path itself (generation 0) is now frozen forever and
	// still contains the aged record; resolve the CURRENT generation the
	// same way OpenInstanceLog/Append/ReadInstanceLog do.
	schedulerDir := filepath.Dir(path)
	currentPath, err := journal.InstanceEventsPath(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if currentPath == path {
		t.Fatalf("compaction did not advance the instance log generation")
	}
	data, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "2020-01-01") {
		t.Fatalf("aged record still present after compaction: %s", data)
	}
}
