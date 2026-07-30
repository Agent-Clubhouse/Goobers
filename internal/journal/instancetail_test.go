package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInstanceEvents writes n well-formed events to an instance journal and
// returns its path.
func writeInstanceEvents(t *testing.T, dir string, n int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileEvents)
	var buf bytes.Buffer
	for i := 1; i <= n; i++ {
		line, err := json.Marshal(Event{Schema: EventSchema, Seq: uint64(i), Type: EventTickSkipped, Reason: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTailSequenceSurvivesNULCascade is §14.11's required coverage, and the
// single most important test of #1914.
//
// readEventRecords strips NUL crash-fill and skips a line that collapses to
// empty, so the #116 cascade can leave a **newline-terminated, fill-only tail
// after the last valid event**. A tail read that stopped at the last newline
// would find that line, yield no event, recover seq=0, and reallocate from 1 —
// duplicating every sequence in the journal, which is #530's original defect.
//
// The scan must keep walking backwards to a line that parses with a non-zero Seq.
func TestTailSequenceSurvivesNULCascade(t *testing.T) {
	dir := t.TempDir()
	path := writeInstanceEvents(t, dir, 12)

	// Append the cascade shape: NUL fill terminated by a newline, so it is a
	// "complete" line by the last-newline rule but carries no event.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(make([]byte, 4096), '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	seq, torn, _, err := tailSequence(path)
	if err != nil {
		t.Fatalf("tailSequence: %v", err)
	}
	if seq != 12 {
		t.Fatalf("recovered seq %d, want 12.\n"+
			"A fill-only tail must not stop the backward scan: recovering 0 here reallocates from 1 and "+
			"duplicates every sequence in the journal (#530, #116).", seq)
	}
	if torn != 0 {
		t.Errorf("torn bytes = %d, want 0: the fill line is newline-terminated, so nothing is torn", torn)
	}
}

// TestTailSequenceMatchesFullReadAcrossShapes pins the bounded scan against the
// full read it replaces, across the journal shapes that actually occur.
//
// This is the differential that matters: the two must agree on both the
// allocated sequence and the torn-tail size, or recovery and allocation diverge
// depending on which path ran.
func TestTailSequenceMatchesFullReadAcrossShapes(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, path string)
		records int
	}{
		{name: "clean", records: 20},
		{name: "single record", records: 1},
		{
			name:    "torn partial tail",
			records: 20,
			mutate: func(t *testing.T, path string) {
				appendRaw(t, path, []byte(`{"schema":"goobers.dev/journal/v1","seq":`))
			},
		},
		{
			name:    "nul fill, unterminated",
			records: 20,
			mutate: func(t *testing.T, path string) {
				appendRaw(t, path, make([]byte, 300))
			},
		},
		{
			name:    "nul fill terminated then partial",
			records: 20,
			mutate: func(t *testing.T, path string) {
				appendRaw(t, path, append(make([]byte, 200), '\n'))
				appendRaw(t, path, []byte(`{"seq":`))
			},
		},
		{
			name:    "record larger than one chunk",
			records: 5,
			mutate: func(t *testing.T, path string) {
				line, err := json.Marshal(Event{
					Schema: EventSchema, Seq: 6, Type: EventError,
					Error: &ErrorDetail{Code: "big", Message: strings.Repeat("y", tailChunkSize*3)},
				})
				if err != nil {
					t.Fatal(err)
				}
				appendRaw(t, path, append(line, '\n'))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeInstanceEvents(t, dir, tc.records)
			if tc.mutate != nil {
				tc.mutate(t, path)
			}

			wantEvents, wantTorn, err := readEvents(path)
			if err != nil {
				t.Fatalf("full read: %v", err)
			}
			wantSeq := highestEventSeq(wantEvents)

			gotSeq, gotTorn, _, err := tailSequence(path)
			if err != nil {
				t.Fatalf("tail read: %v", err)
			}
			if gotSeq != wantSeq {
				t.Errorf("tail read allocated seq %d, full read %d", gotSeq, wantSeq)
			}
			if gotTorn != wantTorn {
				t.Errorf("tail read torn bytes %d, full read %d", gotTorn, wantTorn)
			}
		})
	}
}

// TestTailSequenceNeverAllocatesFromZeroOnExhaustedBudget pins the rule the
// design states in the strongest terms: an exhausted budget must fall back to a
// full read, "**rather than allocating from zero**".
//
// Allocating from zero is not a degraded answer, it is the #530 defect — every
// subsequent sequence duplicates one already in the journal.
func TestTailSequenceNeverAllocatesFromZeroOnExhaustedBudget(t *testing.T) {
	dir := t.TempDir()
	path := writeInstanceEvents(t, dir, 3)

	// A fill region larger than the budget with no parseable event inside it, made
	// of many newline-terminated fill lines rather than one enormous one — a
	// single line above maxEventBytes is corruption the FULL read also rejects,
	// so it would test the scanner's line limit rather than the budget fallback.
	fillLine := append(make([]byte, 1024), '\n')
	var fill []byte
	for len(fill) < instanceTailBudget+2048 {
		fill = append(fill, fillLine...)
	}
	appendRaw(t, path, fill)

	if _, _, _, err := tailSequence(path); err == nil {
		t.Fatal("expected errTailBudgetExhausted when no event is reachable within the budget")
	}

	// And the caller's fallback must recover the real sequence.
	log, _, err := OpenInstanceLog(dir)
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	if err := log.Append(Event{Type: EventTickSkipped, Reason: "after exhaustion"}); err != nil {
		t.Fatalf("append after budget exhaustion: %v", err)
	}
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatalf("read instance log: %v", err)
	}
	var maxSeq uint64
	seen := map[uint64]int{}
	for _, ev := range events {
		seen[ev.Seq]++
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
	}
	for seq, n := range seen {
		if n > 1 {
			t.Fatalf("sequence %d appears %d times: the fallback allocated a duplicate", seq, n)
		}
	}
	if maxSeq <= 3 {
		t.Fatalf("highest sequence is %d; the append after exhaustion did not advance past the existing records", maxSeq)
	}
}

// TestInstanceLogAppendReadsBoundedBytes is §14.11's bytes-read-per-append bound.
//
// Before #1914 every append read 100% of the journal, so N appends read N times
// the file. This asserts the bound directly, in bytes, because a duration cannot
// be defended on a contended machine.
func TestInstanceLogAppendReadsBoundedBytes(t *testing.T) {
	dir := t.TempDir()
	// A journal comfortably larger than one chunk, so "bounded" is a real claim
	// rather than an artifact of a tiny file.
	writeInstanceEvents(t, dir, 20_000)
	info, err := os.Stat(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	size := info.Size()
	if size < 4*tailChunkSize {
		t.Fatalf("fixture journal is only %d bytes; too small to demonstrate a bound", size)
	}

	log, _, err := OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	for i := 0; i < 5; i++ {
		if err := log.Append(Event{Type: EventTickSkipped, Reason: "bounded"}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// The bound: one append reads at most a small multiple of the chunk size,
	// regardless of journal size. Asserted against the file rather than a
	// constant so it stays meaningful as the fixture grows.
	if _, _, bytesRead, err := log.allocateSeqFromTail(filepath.Join(dir, fileEvents)); err != nil {
		t.Fatalf("allocate: %v", err)
	} else if int64(bytesRead) > 4*tailChunkSize {
		t.Errorf("an append read %d bytes of a %d-byte journal; the bound is %d",
			bytesRead, size, 4*tailChunkSize)
	} else {
		t.Logf("append read %d bytes of a %d-byte journal (%.2f%%)",
			bytesRead, size, 100*float64(bytesRead)/float64(size))
	}
}

// TestInstanceLogSequenceSurvivesNULCascadeEndToEnd is the same #116 shape as
// TestTailSequenceSurvivesNULCascade, but through the public Append path, so the
// property is pinned where it is actually relied upon.
func TestInstanceLogSequenceSurvivesNULCascadeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := writeInstanceEvents(t, dir, 7)
	appendRaw(t, path, append(make([]byte, 2048), '\n'))

	log, _, err := OpenInstanceLog(dir)
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	for i := 0; i < 3; i++ {
		if err := log.Append(Event{Type: EventTickSkipped, Reason: fmt.Sprintf("after cascade %d", i)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatalf("read instance log: %v", err)
	}
	seen := map[uint64]int{}
	for _, ev := range events {
		seen[ev.Seq]++
	}
	for seq, n := range seen {
		if n > 1 {
			t.Errorf("sequence %d appears %d times after a NUL cascade: allocation restarted", seq, n)
		}
	}
}

// appendRaw appends bytes to a file without interpretation.
func appendRaw(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
