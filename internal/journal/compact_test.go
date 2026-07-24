package journal

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeRawInstanceLog(t *testing.T, dir string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func eventLine(seq int, t time.Time, extra string) string {
	base := `{"schema":"goobers.dev/journal/event/v1","seq":` +
		strconv.Itoa(seq) + `,"time":"` + t.UTC().Format(time.RFC3339Nano) + `","type":"trigger.fired"`
	if extra != "" {
		base += "," + extra
	}
	return base + "}"
}

func TestCompactInstanceEventsDropsAgedRecords(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeRawInstanceLog(t, dir,
		eventLine(1, old, ""),
		eventLine(2, old, ""),
		eventLine(3, recent, `"custom":"keep-me"`),
	)
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	result, err := CompactInstanceEvents(dir, cutoff, false)
	if err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	if result.Dropped != 2 || result.Kept != 1 {
		t.Fatalf("compaction = %+v, want Dropped 2 Kept 1", result)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, `"seq":1`) || strings.Contains(body, `"seq":2`) {
		t.Fatalf("aged records still present: %s", body)
	}
	// The surviving record must keep its ORIGINAL bytes, including fields the
	// compactor never parses (forward-compat preservation).
	if !strings.Contains(body, `"seq":3`) || !strings.Contains(body, `"custom":"keep-me"`) {
		t.Fatalf("kept record lost its raw content: %s", body)
	}
	if result.AfterBytes >= result.BeforeBytes {
		t.Fatalf("AfterBytes %d not smaller than BeforeBytes %d", result.AfterBytes, result.BeforeBytes)
	}
}

func TestCompactInstanceEventsDryRunLeavesFile(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeRawInstanceLog(t, dir, eventLine(1, old, ""), eventLine(2, old, ""))
	before, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	result, err := CompactInstanceEvents(dir, cutoff, true)
	if err != nil {
		t.Fatalf("CompactInstanceEvents dry-run: %v", err)
	}
	if result.Dropped != 2 {
		t.Fatalf("dry-run Dropped = %d, want 2", result.Dropped)
	}
	after, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry-run modified the journal")
	}
}

func TestCompactInstanceEventsKeepsAllWhenNothingAged(t *testing.T) {
	dir := t.TempDir()
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeRawInstanceLog(t, dir, eventLine(1, recent, ""), eventLine(2, recent, ""))
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	result, err := CompactInstanceEvents(dir, cutoff, false)
	if err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	if result.Dropped != 0 || result.Kept != 2 {
		t.Fatalf("compaction = %+v, want Dropped 0 Kept 2", result)
	}
}

func TestCompactInstanceEventsMissingJournal(t *testing.T) {
	dir := t.TempDir()
	result, err := CompactInstanceEvents(dir, time.Now(), false)
	if err != nil {
		t.Fatalf("CompactInstanceEvents on missing journal: %v", err)
	}
	if result.Dropped != 0 || result.Kept != 0 {
		t.Fatalf("missing journal = %+v, want zero", result)
	}
}

func TestCompactInstanceEventsPreservesTornTail(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// A complete aged record, a complete recent record, then a torn partial.
	body := eventLine(1, old, "") + "\n" + eventLine(2, recent, "") + "\n" + `{"seq":3,"time":"2026`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	if _, err := CompactInstanceEvents(dir, cutoff, false); err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if strings.Contains(out, `"seq":1`) {
		t.Fatalf("aged record kept: %s", out)
	}
	if !strings.Contains(out, `{"seq":3,"time":"2026`) {
		t.Fatalf("torn tail not preserved: %s", out)
	}
}
