package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// appendInstanceJournalFixture adds repeated history for sequential reader
// tests. Production Append re-parses the journal tail to allocate every
// sequence; doing that for hundreds of irrelevant filler events dominates
// stress repetitions without exercising the read behavior under test (#3167).
// Call only during fixture setup with no concurrent writer. Meaningful events
// added after a reader's watermark must still use the production append path.
func appendInstanceJournalFixture(t *testing.T, log *journal.InstanceLog, count int, event journal.Event) {
	t.Helper()
	previous, err := journal.ReadInstanceLog(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var seq uint64
	for _, ev := range previous {
		seq = max(seq, ev.Seq)
	}
	event.Schema = journal.EventSchema
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	for range count {
		seq++
		event.Seq = seq
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	path, err := journal.InstanceEventsPath(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data.Bytes()); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
