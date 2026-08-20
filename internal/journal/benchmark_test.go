package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var (
	benchmarkEventBytes   []byte
	benchmarkEventRecord  Event
	benchmarkEventRecords []EventRecord
)

func BenchmarkEventAppendEncode(b *testing.B) {
	secret := []byte(`resolver-token-"with-escaping"`)
	registry, scrubber := DefaultScrubber()
	registry.Register(secret)
	event := benchmarkJournalEvent(string(secret))

	b.Run("EncodeAndScrub", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			line, err := marshalEvent(event)
			if err != nil {
				b.Fatal(err)
			}
			benchmarkEventBytes = scrubber.Scrub(line)
		}
	})

	b.Run("DurableAppend", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "events.jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = f.Close() })

		var seq uint64
		now := func() time.Time { return time.Unix(0, 0).UTC() }
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			benchmarkEventRecord, err = appendEvent(f, &seq, scrubber, now, event)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkEventParse(b *testing.B) {
	_, scrubber := DefaultScrubber()
	line, err := marshalEvent(benchmarkJournalEvent("ordinary output"))
	if err != nil {
		b.Fatal(err)
	}
	line = append(scrubber.Scrub(line), '\n')
	data := bytes.Repeat(line, 32)
	path := filepath.Join(b.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		benchmarkEventRecords, _, err = readEventRecords(path)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkJournalEvent(output string) Event {
	return Event{
		Schema:  EventSchema,
		Seq:     12,
		Type:    EventStageFinished,
		Branch:  2,
		Time:    time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC),
		Stage:   "implement",
		Attempt: 2,
		Status:  "success",
		Outputs: map[string]any{
			"summary": output,
			"status":  "complete",
		},
		Artifacts: []Ref{{
			Path:      "artifacts/sha256/example",
			Digest:    "sha256:0123456789abcdef",
			Size:      1024,
			MediaType: "text/plain",
		}},
	}
}
