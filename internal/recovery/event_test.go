package recovery

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestRetainedEventSurvivesInstanceJournalRoundTrip(t *testing.T) {
	record := storageTestRecord()
	record.BaseRef = "refs/heads/master"
	event, err := RetainedEvent(record)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	log, _, err := journal.OpenInstanceLog(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(event); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(directory)
	if err != nil || len(events) != 1 {
		t.Fatalf("retained event missing: %v %v", events, err)
	}
	got := events[0]
	if got.RunID != record.RunID || got.Type != journal.EventRunnerAnnotation {
		t.Fatalf("wrong recovery event owner/type: %+v", got)
	}
	for key, expected := range map[string]string{
		"recoveryRef": record.Ref, "recoveryPatchDigest": record.PatchDigest,
		"recoveryBaseRef": record.BaseRef, "recoveryBaseSHA": record.BaseSHA, "recoveryRetainUntil": record.RetainUntil.UTC().Format(time.RFC3339Nano),
	} {
		if got.Runner[key] != expected {
			t.Fatalf("lost recovery field %s: %v", key, got.Runner[key])
		}
	}
}

func TestRetainedEventRefusesUnpublishedRecord(t *testing.T) {
	record := storageTestRecord()
	record.ArchiveDigest, record.ArchiveBytes = "", 0
	if _, err := RetainedEvent(record); err == nil {
		t.Fatal("unbound snapshot admitted as retained evidence")
	}
}
