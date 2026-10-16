package journal

import (
	"os"
	"runtime"
	"testing"
)

// TestOpenInstanceLogRecoversFromBoundedTail pins the startup/per-operation
// open to the same bounded tail read Append uses. Open holds the journal lock
// every Append needs, so a full parse of a large journal stalls all writers.
func TestOpenInstanceLogRecoversFromBoundedTail(t *testing.T) {
	dir := t.TempDir()
	const events = 40000
	path := writeInstanceEvents(t, dir, events)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	log, report, err := OpenInstanceLog(dir)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	if report.LastSeq != events || report.TornBytes != 0 || report.Repaired {
		t.Fatalf("report = %+v, want LastSeq=%d and no repair", report, events)
	}
	// A full parse allocates many times the file size; the tail read is a small
	// fraction of it.
	if allocated := int64(after.TotalAlloc - before.TotalAlloc); allocated > info.Size()/2 {
		t.Fatalf("OpenInstanceLog allocated %d bytes for a %d-byte journal; want a bounded tail read", allocated, info.Size())
	}

	if err := log.Append(Event{Type: EventTickSkipped, Reason: "after open"}); err != nil {
		t.Fatal(err)
	}
	all, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := all[len(all)-1].Seq; got != events+1 {
		t.Fatalf("next seq = %d, want %d", got, events+1)
	}
}

func TestOpenInstanceLogRepairsTornTailFromBoundedTail(t *testing.T) {
	dir := t.TempDir()
	path := writeInstanceEvents(t, dir, 50)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	const torn = `{"schema":"goobers.dev/journal/event/v1","seq":51,"ty`
	if _, err := f.WriteString(torn); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	log, report, err := OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if report.LastSeq != 50 || report.TornBytes != len(torn) || !report.Repaired {
		t.Fatalf("report = %+v, want LastSeq=50 TornBytes=%d Repaired", report, len(torn))
	}
	all, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if last := all[len(all)-1]; last.Type != EventRepaired || last.Seq != 51 {
		t.Fatalf("last event = %s seq %d, want repaired seq 51", last.Type, last.Seq)
	}
}
