package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestLatestGenerationReadDoesNotMaterializeHistory(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.SchedulerDir(), "events.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for i := 1; i <= 32768; i++ {
		if err := enc.Encode(journal.Event{Schema: journal.EventSchema, Seq: uint64(i), Type: journal.EventTickSkipped, Reason: strings.Repeat("x", 1024)}); err != nil {
			t.Fatal(err)
		}
	}
	const generation = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := enc.Encode(journal.Event{Schema: journal.EventSchema, Seq: 32769, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": appliedGenerationAnnotation, "configGeneration": generation}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	got, err := lastAppliedExecutionGeneration(layout)
	runtime.ReadMemStats(&after)
	if err != nil || got != generation {
		t.Fatalf("got=%q err=%v", got, err)
	}
	elapsed := time.Since(started)
	if elapsed > 5*time.Second {
		t.Errorf("tail authority lookup took %s; want at most 5s", elapsed)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("journal_bytes=%d allocated_bytes=%d elapsed=%s", info.Size(), allocated, time.Since(started))
	if allocated > 16<<20 {
		t.Fatalf("latest applied generation is last record, but lookup allocated %d bytes for %d-byte history", allocated, info.Size())
	}
	writeFixture(t, filepath.Join(layout.ConfigDir(), "manifest.yaml"), "config: fixture\n")
	data, digest, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), "fixture-owner")
	if err != nil {
		t.Fatal(err)
	}
	retainer, err := newExecutionGenerationRetainer(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = retainer.Close() }()
	runtime.GC()
	runtime.ReadMemStats(&before)
	started = time.Now()
	for range 10 {
		if _, err := retainer.Keep(t.Context(), data, digest); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	allocated, elapsed = after.TotalAlloc-before.TotalAlloc, time.Since(started)
	t.Logf("ten Keeps: allocated_bytes=%d elapsed=%s", allocated, elapsed)
	if allocated > 16<<20 || elapsed > 5*time.Second {
		t.Fatalf("repeated Keep must not materialize history: allocated=%d elapsed=%s", allocated, elapsed)
	}

}

func TestRetainerKeepsUseIncrementalAppliedGeneration(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	writeFixture(t, filepath.Join(layout.ConfigDir(), "manifest.yaml"), "config: fixture\n")
	data, digest, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), "fixture-owner")
	if err != nil {
		t.Fatal(err)
	}
	retainer, err := newExecutionGenerationRetainer(layout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = retainer.Close() }()
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if err := journalStartupGeneration(log, "A", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := retainer.Keep(t.Context(), data, digest); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{"B", "A", ""} {
		if err := log.Append(journal.Event{Type: journal.EventConfigReloaded, Runner: map[string]any{"configGeneration": generation}}); err != nil {
			t.Fatal(err)
		}
		if _, err := retainer.Keep(t.Context(), data, digest); err != nil {
			t.Fatal(err)
		}
		pins, err := retainer.DurablePins(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if generation == "" && (pins["A"] || pins["B"]) || generation != "" && !pins[generation] {
			t.Fatalf("stale applied protection after %q: %v", generation, pins)
		}
	}
}

func retainedExecutionGenerationPins(ctx context.Context, layout instance.Layout) (map[string]bool, error) {
	return executionGenerationPinsReader(layout)(ctx)
}
