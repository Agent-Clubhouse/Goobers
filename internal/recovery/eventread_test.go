package recovery

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

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
