package journal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstanceLatestTracksAppendsLegacyBarrierAndReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileEvents)
	lookup := NewInstanceLatest(dir, func(e Event) bool { return e.Type == EventConfigReloaded })
	check := func(want string, found bool) {
		t.Helper()
		event, got, err := lookup.Read(t.Context())
		generation, _ := event.Runner["configGeneration"].(string)
		if err != nil || got != found || generation != want {
			t.Fatalf("latest = %q, %v, %v; want %q, %v", generation, got, err, want, found)
		}
	}
	check("", false)
	writeLatestFixture(t, path, false, latestApplied("A"), Event{Type: EventTickSkipped})
	check("A", true)
	check("A", true)
	writeLatestFixture(t, path, true, latestApplied("B"))
	check("B", true)
	// The generation-less event is a barrier, not permission to reuse A or B.
	writeLatestFixture(t, path, true, Event{Type: EventConfigReloaded})
	check("", true)
	// Replacement at the same path must discard the cached barrier and offset.
	replacement := filepath.Join(dir, "replacement")
	writeLatestFixture(t, replacement, false, latestApplied("C"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	check("C", true)
	// A same-size rewrite of the same inode invalidates via modification time.
	writeLatestFixture(t, path, false, latestApplied("E"))
	changed := time.Now().Add(time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	check("E", true)
	// Compaction can advance the generation even with an unchanged old file.
	writeLatestFixture(t, filepath.Join(dir, instanceEventsFilename(1)), false, latestApplied("D"))
	if err := advanceInstanceEventsPointer(dir, 1); err != nil {
		t.Fatal(err)
	}
	check("D", true)
	if err := os.Truncate(filepath.Join(dir, instanceEventsFilename(1)), 0); err != nil {
		t.Fatal(err)
	}
	check("", false)
}

func TestInstanceLatestCrossesChunksAndRetainsTornBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileEvents)
	lookup := NewInstanceLatest(dir, func(e Event) bool { return e.Type == EventConfigReloaded })
	// The authority is older than multiple chunks, including a spanning record.
	writeLatestFixture(t, path, false, latestApplied("A"), Event{Type: EventTickSkipped, Reason: strings.Repeat("x", 3*tailChunkSize)}, Event{Type: EventTickSkipped})
	event, found, err := lookup.Read(t.Context())
	if err != nil || !found || event.Runner["configGeneration"] != "A" {
		t.Fatalf("deep lookup: %+v %v %v", event, found, err)
	}
	data, err := json.Marshal(latestApplied("B"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data[:len(data)/2]); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	event, _, err = lookup.Read(t.Context())
	if err != nil || event.Runner["configGeneration"] != "A" {
		t.Fatalf("torn lookup: %+v %v", event, err)
	}
	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(data[len(data)/2:], '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	event, _, err = lookup.Read(t.Context())
	if err != nil || event.Runner["configGeneration"] != "B" {
		t.Fatalf("completed lookup: %+v %v", event, err)
	}
}

func TestInstanceLatestRefusesCorruptionAndCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileEvents)
	lookup := NewInstanceLatest(dir, func(e Event) bool { return e.Type == EventConfigReloaded })
	writeLatestFixture(t, path, false, latestApplied("A"))
	if _, _, err := lookup.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("invalid\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err := lookup.Read(t.Context()); err == nil {
			t.Fatal("corruption cached or ignored")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := lookup.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func latestApplied(generation string) Event {
	return Event{Schema: EventSchema, Type: EventConfigReloaded, Runner: map[string]any{"configGeneration": generation}}
}

func writeLatestFixture(t *testing.T, path string, appendOnly bool, events ...Event) {
	t.Helper()
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if appendOnly {
		flags = os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, event := range events {
		event.Schema = EventSchema
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstanceLatestSkipsCachedHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileEvents)
	seen := 0
	lookup := NewInstanceLatest(dir, func(e Event) bool {
		seen++
		return e.Type == EventConfigReloaded
	})
	writeLatestFixture(t, path, false, latestApplied("A"), Event{Type: EventTickSkipped})
	if _, _, err := lookup.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("cold read decoded %d events", seen)
	}
	if _, _, err := lookup.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("unchanged journal decoded history again: %d", seen)
	}
	writeLatestFixture(t, path, true, Event{Type: EventTickSkipped})
	event, _, err := lookup.Read(t.Context())
	if err != nil || event.Runner["configGeneration"] != "A" || seen != 3 {
		t.Fatalf("append reread history: event=%+v seen=%d err=%v", event, seen, err)
	}
}

func TestInstanceLatestLargeRecordAllocationIsLinear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileEvents)
	writeLatestFixture(t, path, false, latestApplied("A"), Event{Type: EventTickSkipped, Reason: strings.Repeat("x", 6<<20)})
	lookup := NewInstanceLatest(dir, func(e Event) bool { return e.Type == EventConfigReloaded })
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	event, found, err := lookup.Read(t.Context())
	runtime.ReadMemStats(&after)
	if err != nil || !found || event.Runner["configGeneration"] != "A" {
		t.Fatalf("large record lookup: %+v %v %v", event, found, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("spanning record allocated %d bytes; want linear bounded-record buffering", allocated)
	}
}
