package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/platform/safeopen"
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

func TestInstanceLogStateDistinguishesRecreatedGenerationZero(t *testing.T) {
	dir := t.TempDir()
	appendInstanceEvents(t, dir, Event{Type: EventRunnerAnnotation})
	before, err := ReadInstanceLogState(dir)
	if err != nil {
		t.Fatal(err)
	}
	appendInstanceEvents(t, dir, Event{Type: EventTickSkipped})
	continued, err := ReadInstanceLogState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.identity == "" || before.identity != continued.identity || !before.SameJournal(continued) {
		t.Fatalf("continued journal states = %#v, %#v; want one durable identity", before, continued)
	}
	path, err := InstanceEventsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	appendInstanceEvents(t, dir, Event{Type: EventTickSkipped})
	after, err := ReadInstanceLogState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.identity == "" || after.identity == "" {
		t.Fatalf("journal identities = %q, %q; want two durable identities", before.identity, after.identity)
	}
	if before.identity == after.identity {
		t.Fatalf("recreated generation-zero journal retained identity %q", before.identity)
	}
	if before.SameJournal(after) {
		t.Fatal("recreated generation-zero journal matches its prior state")
	}
}

func TestInstanceLogIdentitySurvivesCompaction(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	log, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if err := log.Append(Event{Type: EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	before, err := ReadInstanceLogState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Compact(now.Add(time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, err := ReadInstanceLogState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Generation == after.Generation {
		t.Fatalf("generation remained %d; test did not exercise compaction", before.Generation)
	}
	if before.identity == "" || before.identity != after.identity {
		t.Fatalf("identity across compaction = %q, %q; want one durable identity", before.identity, after.identity)
	}
}

func TestOpenInstanceLogAdoptsLegacyIdentityWithoutLosingEvents(t *testing.T) {
	dir := t.TempDir()
	appendInstanceEvents(t, dir, Event{Type: EventDaemonStarted})
	if err := os.Remove(filepath.Join(dir, fileInstanceLogID)); err != nil {
		t.Fatal(err)
	}
	legacy, err := ReadInstanceLogState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.identity != "" {
		t.Fatalf("legacy identity = %q, want empty", legacy.identity)
	}
	appendInstanceEvents(t, dir, Event{Type: EventTickSkipped})
	adopted, err := ReadInstanceLogState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.identity == "" {
		t.Fatal("writer did not adopt a durable identity for the legacy journal")
	}
	if legacy.SameJournal(adopted) {
		t.Fatal("identity adoption must force an incremental reader to reset")
	}
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events after identity adoption = %d, want 2", len(events))
	}
}

func TestReadInstanceLogIDRejectsUnsafeOrMalformedFiles(t *testing.T) {
	valid := []byte(strings.Repeat("a", 32) + "\n")
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "oversize", data: append(valid, 'x')},
		{name: "malformed", data: []byte(strings.Repeat("g", 32) + "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, fileInstanceLogID), tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readInstanceLogID(dir); err == nil {
				t.Fatal("readInstanceLogID accepted unsafe identity file")
			}
		})
	}

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, valid, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, fileInstanceLogID)); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		if _, err := readInstanceLogID(dir); err == nil {
			t.Fatal("readInstanceLogID followed a symlink")
		}
	})
}

func TestReadInstanceLogIDRejectsFileSwappedWhileOpening(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileInstanceLogID)
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readInstanceLogIDWith(dir, func(parent *os.File, name string) (*os.File, error) {
		if renameErr := os.Rename(path, path+".old"); renameErr != nil {
			return nil, renameErr
		}
		if writeErr := os.WriteFile(path, []byte(strings.Repeat("b", 32)+"\n"), 0o600); writeErr != nil {
			return nil, writeErr
		}
		return safeopen.OpenAt(parent, name)
	})
	if err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("readInstanceLogIDWith swap error = %v, want changed-while-opening refusal", err)
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
