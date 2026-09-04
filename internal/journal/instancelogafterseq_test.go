package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readprobe"
)

func appendInstanceEvents(t *testing.T, dir string, events ...Event) {
	t.Helper()
	log, _, err := OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := log.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadInstanceLogAfterSeqReturnsOnlyLaterEvents(t *testing.T) {
	dir := t.TempDir()
	appendInstanceEvents(t, dir,
		Event{Type: EventDaemonStarted},
		Event{Type: EventTickSkipped, Reason: "first"},
		Event{Type: EventTickSkipped, Reason: "second"},
	)

	all, err := ReadInstanceLogAfterSeq(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("ReadInstanceLogAfterSeq(0) returned %d events, want 3", len(all))
	}

	rest, err := ReadInstanceLogAfterSeq(dir, all[0].Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 || rest[0].Reason != "first" || rest[1].Reason != "second" {
		t.Fatalf("ReadInstanceLogAfterSeq(%d) = %+v, want the two later events", all[0].Seq, rest)
	}

	none, err := ReadInstanceLogAfterSeq(dir, all[len(all)-1].Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("ReadInstanceLogAfterSeq(highest) = %+v, want no events", none)
	}
}

func TestReadInstanceLogAfterSeqToleratesMissingJournal(t *testing.T) {
	events, err := ReadInstanceLogAfterSeq(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("ReadInstanceLogAfterSeq on an absent journal = %v, want no error", err)
	}
	if len(events) != 0 {
		t.Fatalf("ReadInstanceLogAfterSeq on an absent journal = %+v, want no events", events)
	}
}

func TestReadInstanceLogAfterSeqSurfacesCorruption(t *testing.T) {
	dir := t.TempDir()
	appendInstanceEvents(t, dir, Event{Type: EventDaemonStarted})
	path, err := InstanceEventsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if events, err := ReadInstanceLogAfterSeq(dir, 0); err == nil {
		t.Fatalf("ReadInstanceLogAfterSeq over a corrupt journal = %+v, nil; want an error", events)
	}
}

// TestReadInstanceLogAfterSeqReadsBoundedBytes is the growth bound the bounded
// read exists for: reading past the journal's highest sequence must cost a tail
// window, not the journal — and ten times the history must not cost ten times
// the bytes (#3050).
func TestReadInstanceLogAfterSeqReadsBoundedBytes(t *testing.T) {
	padding := strings.Repeat("y", 4<<10)
	measure := func(t *testing.T, count int) (bytesRead uint64, size int64) {
		t.Helper()
		dir := t.TempDir()
		events := make([]Event, count)
		for i := range events {
			events[i] = Event{Type: EventTickSkipped, Reason: padding}
		}
		appendInstanceEvents(t, dir, events...)
		highest, err := ReadInstanceLogAfterSeq(dir, 0)
		if err != nil {
			t.Fatal(err)
		}
		readprobe.Enable()
		t.Cleanup(readprobe.Disable)
		before := readprobe.Take()
		if _, err := ReadInstanceLogAfterSeq(dir, highest[len(highest)-1].Seq); err != nil {
			t.Fatal(err)
		}
		work := readprobe.Take().Sub(before)
		readprobe.Disable()
		info, err := os.Stat(filepath.Join(dir, "events.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return work.InstanceTailBytes, info.Size()
	}

	smallBytes, smallSize := measure(t, 64)
	largeBytes, largeSize := measure(t, 640)
	if largeSize < 10*tailChunkSize {
		t.Fatalf("fixture journal is %d bytes, too small to bound against", largeSize)
	}
	if smallBytes != largeBytes {
		t.Fatalf(
			"bounded read cost grew with history: %d bytes over a %d-byte journal, %d bytes over a %d-byte journal",
			smallBytes, smallSize, largeBytes, largeSize,
		)
	}
	if largeBytes > 2*tailChunkSize {
		t.Fatalf("bounded read = %d bytes, want at most %d", largeBytes, 2*tailChunkSize)
	}
}

func TestReadInstanceLogAfterSeqColdReadScalesWithJournal(t *testing.T) {
	padding := strings.Repeat("y", 4<<10)
	measure := func(t *testing.T, count int) (readprobe.Snapshot, int64) {
		t.Helper()
		dir := t.TempDir()
		path, err := InstanceEventsPath(dir)
		if err != nil {
			t.Fatal(err)
		}
		var journal strings.Builder
		for i := 1; i <= count; i++ {
			line, err := marshalEvent(Event{
				Seq:    uint64(i),
				Schema: EventSchema,
				Time:   time.Unix(int64(i), 0).UTC(),
				Type:   EventTickSkipped,
				Reason: padding,
			})
			if err != nil {
				t.Fatal(err)
			}
			journal.Write(line)
			journal.WriteByte('\n')
		}
		if err := os.WriteFile(path, []byte(journal.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		readprobe.Enable()
		t.Cleanup(readprobe.Disable)
		if _, err := ReadInstanceLogAfterSeq(dir, 0); err != nil {
			t.Fatal(err)
		}
		return readprobe.Take(), info.Size()
	}

	var previous readprobe.Snapshot
	var previousSize int64
	for mb := 4; mb <= 64; mb *= 2 {
		t.Run(fmt.Sprintf("%dMB", mb), func(t *testing.T) {
			work, size := measure(t, mb*250)
			target := int64(mb) << 20
			if size < target*9/10 || size > target*11/10 {
				t.Fatalf("fixture journal is %d bytes, want about %d bytes", size, target)
			}
			if work.InstanceTailReads != 1 || work.InstanceTailRecords != uint64(mb*250) {
				t.Fatalf("cold read work = %+v, want one read and %d parsed records", work, mb*250)
			}
			if previousSize != 0 {
				if work.InstanceTailRecords > 3*previous.InstanceTailRecords ||
					work.InstanceTailBytes > 3*previous.InstanceTailBytes {
					t.Fatalf(
						"cold read work grew super-quadratically: %d records/%d bytes over %d records/%d bytes for journals of %d/%d bytes",
						work.InstanceTailRecords, work.InstanceTailBytes,
						previous.InstanceTailRecords, previous.InstanceTailBytes,
						size, previousSize,
					)
				}
			}
			previous, previousSize = work, size
		})
	}
}
