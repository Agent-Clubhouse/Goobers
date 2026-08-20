package journal

import (
	"io"
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

// currentInstanceEventsPath resolves dir's current-generation events file,
// the same way ReadInstanceLog/Append/Compact do, for tests that need to
// inspect the file directly rather than through the journal API.
func currentInstanceEventsPath(t *testing.T, dir string) string {
	t.Helper()
	path, _, err := resolveInstanceEventsPath(dir)
	if err != nil {
		t.Fatalf("resolveInstanceEventsPath: %v", err)
	}
	return path
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

	result, err := CompactInstanceEvents(dir, cutoff, cutoff, false)
	if err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	if result.Dropped != 2 || result.Kept != 1 {
		t.Fatalf("compaction = %+v, want Dropped 2 Kept 1", result)
	}
	data, err := os.ReadFile(currentInstanceEventsPath(t, dir))
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

func TestCompactInstanceEventsPreservesInitCompletion(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	initCompleted := strings.Replace(eventLine(1, old, ""), string(EventTriggerFired), string(EventInitCompleted), 1)
	writeRawInstanceLog(t, dir,
		initCompleted,
		eventLine(2, old, ""),
		eventLine(3, recent, ""),
	)

	result, err := CompactInstanceEvents(dir, recent.Add(-time.Hour), recent.Add(-time.Hour), false)
	if err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	if result.Dropped != 1 || result.Kept != 2 {
		t.Fatalf("compaction = %+v, want Dropped 1 Kept 2", result)
	}
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != EventInitCompleted || events[1].Seq != 3 {
		t.Fatalf("events after compaction = %#v, want init.completed and recent event", events)
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

	result, err := CompactInstanceEvents(dir, cutoff, cutoff, true)
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

func TestCompactInstanceEventsRejectsUnsupportedSchemaWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	future := `{"schema":"goobers.dev/journal/event/v2","seq":1,"time":"2026-01-01T00:00:00Z","type":"future.event"}`
	writeRawInstanceLog(t, dir, future)
	path := filepath.Join(dir, fileEvents)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := CompactInstanceEvents(dir, time.Now(), time.Time{}, false); err == nil {
		t.Fatal("CompactInstanceEvents accepted an unsupported event schema")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("CompactInstanceEvents mutated the journal before rejecting its schema")
	}
}

func TestCompactInstanceEventsKeepsAllWhenNothingAged(t *testing.T) {
	dir := t.TempDir()
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeRawInstanceLog(t, dir, eventLine(1, recent, ""), eventLine(2, recent, ""))
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	result, err := CompactInstanceEvents(dir, cutoff, cutoff, false)
	if err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	if result.Dropped != 0 || result.Kept != 2 {
		t.Fatalf("compaction = %+v, want Dropped 0 Kept 2", result)
	}
}

func TestCompactInstanceEventsMissingJournal(t *testing.T) {
	dir := t.TempDir()
	result, err := CompactInstanceEvents(dir, time.Now(), time.Now(), false)
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

	if _, err := CompactInstanceEvents(dir, cutoff, cutoff, false); err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	data, err := os.ReadFile(currentInstanceEventsPath(t, dir))
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

// TestInstanceLogCompactKeepsOpenWritersOnActiveJournal pins #2265's
// generation scheme: compaction never rewrites a path any reader might have
// open (see instancegen.go). `before` models exactly that — an ordinary
// os.Open handle on the pre-compaction generation, held open across
// Compact() — the scenario that deadlocked both a MoveFileEx-based and a
// ReplaceFile-based in-place rewrite on real Windows CI, since neither API
// can act on a path with an open handle lacking FILE_SHARE_DELETE regardless
// of how "tolerant" it claims to be. The generation scheme sidesteps the
// restriction entirely: `before`'s path is never touched again, by anyone.
func TestInstanceLogCompactKeepsOpenWritersOnActiveJournal(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	if err := first.Append(Event{Type: EventTickSkipped, Workflow: "old"}); err != nil {
		t.Fatal(err)
	}
	preCompactPath := currentInstanceEventsPath(t, dir)
	// An ordinary reader, exactly the way the portal/CLI/anything outside
	// this package would open events.jsonl — no special sharing flags, held
	// open across the compaction below.
	before, err := os.Open(preCompactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = before.Close() }()
	now = now.Add(48 * time.Hour)
	if err := second.Append(Event{Type: EventTickSkipped, Workflow: "recent"}); err != nil {
		t.Fatal(err)
	}
	result, err := first.Compact(now.Add(-24*time.Hour), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if result.Dropped != 1 || result.Kept != 1 {
		t.Fatalf("compaction = %+v, want one dropped and one kept", result)
	}

	// The pre-compaction generation is never touched again: even a brand-new
	// open of its path (not just the already-open `before` handle) still
	// sees the frozen pre-compaction content.
	sealed, err := os.ReadFile(preCompactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sealed), `"workflow":"old"`) {
		t.Fatalf("previous generation was modified after compaction: %s", sealed)
	}
	oldData, err := io.ReadAll(before)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(oldData), `"workflow":"old"`) {
		t.Fatalf("already-open reader lost its content: %s", oldData)
	}

	// The current generation reflects the compacted state.
	postCompactPath := currentInstanceEventsPath(t, dir)
	if postCompactPath == preCompactPath {
		t.Fatalf("compaction did not advance the generation: still %s", postCompactPath)
	}
	current, err := os.ReadFile(postCompactPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(current), `"workflow":"old"`) {
		t.Fatalf("current generation still contains the aged-out event: %s", current)
	}
	if !strings.Contains(string(current), `"workflow":"recent"`) {
		t.Fatalf("current generation lost the kept event: %s", current)
	}

	// A writer opened before the compaction keeps working transparently —
	// ensureActiveFile resolves the pointer fresh on every Append and
	// reopens when it has moved.
	if err := second.Append(Event{Type: EventTickSkipped, Workflow: "after"}); err != nil {
		t.Fatalf("append through independently opened handle after compaction: %v", err)
	}

	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Workflow != "recent" || events[1].Workflow != "after" || events[1].Seq != 3 {
		t.Fatalf("events after live compaction = %#v", events)
	}
}

func TestInstanceLogCompactPreservesLatestScheduledTriggerPerWorkflow(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeRawInstanceLog(t, dir,
		eventLine(1, old, `"gaggle":"g","workflow":"monthly","reason":"scheduled"`),
		eventLine(2, old.Add(time.Hour), `"gaggle":"g","workflow":"monthly","reason":"catch-up (missed 2)"`),
		eventLine(3, old, `"gaggle":"g","workflow":"manual","reason":"manual"`),
		eventLine(4, recent, `"workflow":"recent","reason":"scheduled"`),
	)

	if _, err := CompactInstanceEvents(dir, recent.Add(-time.Hour), recent.Add(-time.Hour), false); err != nil {
		t.Fatal(err)
	}
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Seq != 2 || events[1].Seq != 4 {
		t.Fatalf("retained trigger checkpoints = %#v, want seq 2 and 4", events)
	}
}

func TestInstanceLogCompactionBoundsSustainedTickJournal(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	log, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	const ticks = 500
	for i := 0; i < ticks; i++ {
		now = now.Add(time.Second)
		if err := log.Append(Event{Type: EventTickSkipped, Workflow: "implement", Reason: "conditions: max-parallel"}); err != nil {
			t.Fatalf("append tick %d: %v", i, err)
		}
		if (i+1)%50 == 0 {
			if _, err := log.Compact(now.Add(-10*time.Second), now.Add(-10*time.Second)); err != nil {
				t.Fatalf("compact after tick %d: %v", i, err)
			}
		}
	}

	info, err := os.Stat(currentInstanceEventsPath(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 8*1024 {
		t.Fatalf("steady-state journal grew to %d bytes after %d ticks", info.Size(), ticks)
	}
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) > 11 {
		t.Fatalf("steady-state journal retained %d events, want at most 11", len(events))
	}

	// 10 compactions (500 ticks / 50) advance the generation 10 times;
	// cleanupStaleInstanceEventsGeneration must keep this bounded rather than
	// accumulating one file per compaction forever.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	eventsFiles := 0
	for _, entry := range entries {
		if entry.Name() == fileEvents || strings.HasPrefix(entry.Name(), fileEvents+".gen-") {
			eventsFiles++
		}
	}
	if eventsFiles > 2 {
		t.Fatalf("dir retained %d events-file generations after sustained compaction, want at most 2: %v", eventsFiles, entries)
	}
}
