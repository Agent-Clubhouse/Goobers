package recovery

import (
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestRecoveryEventFoldUsesSupportedInventoryBound(t *testing.T) {
	record := storageTestRecord()
	events := make([]journal.Event, 129)
	for i := range events {
		current := record
		current.SnapshotSHA = fmt.Sprintf("%040x", i+1)
		var err error
		current.Ref, err = RefForSnapshot(current.RunID, current.SnapshotSHA)
		if err != nil {
			t.Fatal(err)
		}
		events[i], err = RetainedEvent(current)
		if err != nil {
			t.Fatal(err)
		}
	}
	if records, err := RecordsFromEvents(events, record.RunID); err != nil || len(records) != 129 {
		t.Fatalf("supported observation set: records=%d err=%v", len(records), err)
	}
	if records, err := RecordsFromEventsBounded(events, record.RunID, 128); err == nil || records != nil {
		t.Fatalf("configured overflow returned partial records: records=%d err=%v", len(records), err)
	}
}

// TestRecoveryEventFoldRoundTripsArchiveFormat pins #4862: a journal-observed
// record must decode with the same ArchiveFormat the disk record carries, or
// every later comparison against that disk record (capture ordering,
// abandonment evidence) spuriously reports a conflict.
func TestRecoveryEventFoldRoundTripsArchiveFormat(t *testing.T) {
	record := storageTestRecord()
	record.BaseRef = "refs/heads/master"
	record.ArchiveFormat = archiveFormatDelta
	event, err := RetainedEvent(record)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RecordsFromEvents([]journal.Event{event}, record.RunID)
	if err != nil || len(got) != 1 || got[0] != record {
		t.Fatalf("recovery fold lost archive format = %+v %v", got, err)
	}
}

func TestRecoveryEventFoldKeepsLatestWindowAndRejectsSpoofs(t *testing.T) {
	record := storageTestRecord()
	record.BaseRef = "refs/heads/master"
	initial, err := RetainedEvent(record)
	if err != nil {
		t.Fatal(err)
	}
	renewed := record
	renewed.RetainUntil = record.RetainUntil.Add(time.Hour)
	later, _ := RetainedEvent(renewed)
	spoof, _ := RetainedEvent(record)
	spoof.Runner[livejournal.EmitKeyRunnerField] = "stage-emission"
	spoof.Runner["recoveryRef"] = "invalid but ignored emission"
	got, err := RecordsFromEvents([]journal.Event{initial, later, initial, spoof}, record.RunID)
	if err != nil || len(got) != 1 || got[0] != renewed {
		t.Fatalf("recovery fold = %+v %v", got, err)
	}
	conflict := record
	conflict.ArchiveBytes++
	other, _ := RetainedEvent(conflict)
	if got, err := RecordsFromEvents([]journal.Event{initial, other}, record.RunID); err == nil || got != nil {
		t.Fatalf("conflicting custody evidence accepted: %+v %v", got, err)
	}
}

func TestRecoveryEventFoldRejectsMissingFieldsAndForeignRun(t *testing.T) {
	record := storageTestRecord()
	event, _ := RetainedEvent(record)
	if got, err := RecordsFromEvents([]journal.Event{event}, "different"); err == nil || got != nil {
		t.Fatalf("foreign event accepted: %+v %v", got, err)
	}
	delete(event.Runner, "recoveryCreatedAt")
	if got, err := RecordsFromEvents([]journal.Event{event}, record.RunID); err == nil || got != nil {
		t.Fatalf("incomplete event accepted: %+v %v", got, err)
	}
}
