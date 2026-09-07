package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNoDropCompactionDoesNotScaleWithJournalHistory is #3051's work fence.
//
// The six-hourly pass read and JSON-parsed the entire current generation
// before it could discover that nothing had aged out, so its cost grew with
// journal history forever while its output stayed empty. The oldest record
// alone settles that question.
//
// The assertion is a ratio, not a byte count: a no-drop pass over a large
// journal must read a small, bounded prefix of it. Pinning an absolute number
// would just re-encode the read buffer's size.
func TestNoDropCompactionDoesNotScaleWithJournalHistory(t *testing.T) {
	dir := t.TempDir()
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	lines := make([]string, 0, 5000)
	for seq := 1; seq <= 5000; seq++ {
		lines = append(lines, eventLine(seq, recent, `"padding":"`+strings.Repeat("x", 200)+`"`))
	}
	writeRawInstanceLog(t, dir, lines...)

	info, err := os.Stat(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	// Every record is newer than the cut, so nothing can age out.
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	result, err := CompactInstanceEvents(dir, cutoff, cutoff, false)
	if err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	if result.Dropped != 0 {
		t.Fatalf("compaction = %+v, want Dropped 0", result)
	}
	if result.BytesRead >= info.Size()/10 {
		t.Fatalf("a no-drop pass read %d of %d bytes; it must not scale with journal history",
			result.BytesRead, info.Size())
	}
}

// TestFencedCompactionStillDropsWhenTheOldestRecordIsAged is the other half:
// the fence must not suppress a pass that has real work. The oldest record is
// aged, so the full pass runs and the aged records go.
func TestFencedCompactionStillDropsWhenTheOldestRecordIsAged(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	writeRawInstanceLog(t, dir,
		eventLine(1, old, ""),
		eventLine(2, old, ""),
		eventLine(3, recent, ""),
	)
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	result, err := CompactInstanceEvents(dir, cutoff, cutoff, false)
	if err != nil {
		t.Fatalf("CompactInstanceEvents: %v", err)
	}
	if result.Dropped != 2 || result.Kept != 1 {
		t.Fatalf("compaction = %+v, want Dropped 2 Kept 1: the fence must not suppress real work", result)
	}
	if result.BytesRead == 0 {
		t.Fatal("BytesRead = 0 on a pass that parsed the journal")
	}
}

// TestFenceFallsThroughOnAMalformedFirstRecord keeps the fence out of the
// error contract. A journal whose first record cannot be parsed, or carries an
// unsupported schema, must still reach the full pass that owns those
// diagnostics — the fence may skip work, never swallow a failure.
func TestFenceFallsThroughOnAMalformedFirstRecord(t *testing.T) {
	for _, tt := range []struct {
		name  string
		first string
	}{
		{name: "unsupported schema", first: `{"schema":"goobers.dev/journal/event/v2","seq":1,"time":"2026-07-01T00:00:00Z","type":"future.event"}`},
		{name: "undecodable", first: `{"schema":"goobers.dev/journal/event/v1","seq":1,`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			recent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
			writeRawInstanceLog(t, dir, tt.first, eventLine(2, recent, ""))
			cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

			if _, err := CompactInstanceEvents(dir, cutoff, cutoff, false); err == nil {
				t.Fatal("CompactInstanceEvents err = nil; the fence must not hide a malformed journal " +
					"by skipping the pass that reports it")
			}
		})
	}
}
