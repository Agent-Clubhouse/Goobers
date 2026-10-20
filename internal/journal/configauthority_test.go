package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompactionPreservesLatestAuthorityBarrier(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileEvents)
	old, recent := time.Now().Add(-48*time.Hour), time.Now()
	events := []Event{
		{Schema: EventSchema, Seq: 1, Time: old, Type: EventTickSkipped},
		{Schema: EventSchema, Seq: 2, Time: recent, Type: EventConfigReloaded, Runner: map[string]any{"configGeneration": "A"}},
		{Schema: EventSchema, Seq: 3, Time: old, Type: EventConfigReloaded},
		{Schema: EventSchema, Seq: 4, Time: recent, Type: EventTickSkipped},
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	reader := NewInstanceLatest(dir, func(e Event) bool { return e.Type == EventConfigReloaded })
	before, found, err := reader.Read(t.Context())
	if err != nil || !found || before.Seq != 3 {
		t.Fatalf("before=%+v found=%v err=%v", before, found, err)
	}
	cutoff := recent.Add(-24 * time.Hour)
	if _, err := CompactInstanceEvents(dir, cutoff, cutoff, false); err != nil {
		t.Fatal(err)
	}
	after, found, err := reader.Read(t.Context())
	if err != nil || !found || after.Seq != 3 {
		t.Fatalf("compaction resurrected superseded authority: after=%+v found=%v err=%v", after, found, err)
	}
}

func TestCompactionRetainsAuthorityRawBytesAcrossEventTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		last Event
	}{
		{"startup", Event{Type: EventRunnerAnnotation, Runner: map[string]any{"kind": ConfigGenerationAppliedAnnotation, "configGeneration": "A"}}},
		{"reload", Event{Type: EventConfigReloaded, Runner: map[string]any{"configGeneration": "A"}}},
		{"legacy-barrier", Event{Type: EventConfigReloaded}},
	} {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/live=%v", tc.name, live), func(t *testing.T) {
				dir := t.TempDir()
				old, recent := time.Now().Add(-48*time.Hour), time.Now()
				events := []Event{
					{Type: EventConfigReloaded, Runner: map[string]any{"configGeneration": "B"}},
					{Type: EventRunnerAnnotation, Runner: map[string]any{"kind": ConfigGenerationAppliedAnnotation, "configGeneration": "C"}},
					tc.last,
					{Type: EventTickSkipped},
				}
				var lines []string
				for i, event := range events {
					event.Schema, event.Seq, event.Time = EventSchema, uint64(i+1), old
					if i == 3 {
						event.Time = recent
					}
					data, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					// Unknown data must survive byte-for-byte, not be re-marshaled.
					lines = append(lines, strings.TrimSuffix(string(data), "}")+`, "future": {"preserve":true}}`)
				}
				writeRawInstanceLog(t, dir, lines...)
				cutoff := recent.Add(-24 * time.Hour)
				preview, err := CompactInstanceEvents(dir, cutoff, cutoff, true)
				if err != nil || preview.Kept != 2 || preview.Dropped != 2 {
					t.Fatalf("preview=%+v err=%v", preview, err)
				}
				data, err := os.ReadFile(currentInstanceEventsPath(t, dir))
				if err != nil || string(data) != strings.Join(lines, "\n")+"\n" {
					t.Fatalf("preview changed source: %s %v", data, err)
				}
				reader := NewInstanceLatest(dir, IsConfigGenerationAuthority)
				if event, found, err := reader.Read(t.Context()); err != nil || !found || event.Seq != 3 {
					t.Fatalf("before=%+v %v %v", event, found, err)
				}
				var result InstanceEventsCompaction
				if live {
					log, _, err := OpenInstanceLog(dir)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = log.Close() }()
					result, err = log.Compact(cutoff, cutoff)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					result, err = CompactInstanceEvents(dir, cutoff, cutoff, false)
					if err != nil {
						t.Fatal(err)
					}
				}
				if result.Kept != 2 || result.Dropped != 2 {
					t.Fatalf("result=%+v", result)
				}
				data, err = os.ReadFile(currentInstanceEventsPath(t, dir))
				if err != nil || string(data) != lines[2]+"\n"+lines[3]+"\n" {
					t.Fatalf("authority bytes/order changed: %s %v", data, err)
				}
				if event, found, err := reader.Read(t.Context()); err != nil || !found || event.Seq != 3 {
					t.Fatalf("after=%+v %v %v", event, found, err)
				}
			})
		}
	}
}
