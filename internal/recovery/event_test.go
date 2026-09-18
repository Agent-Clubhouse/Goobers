package recovery

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestRetainedEventSurvivesInstanceJournalRoundTrip(t *testing.T) {
	record := storageTestRecord()
	record.BaseRef = "refs/heads/master"
	record.ArchiveFormat = archiveFormatDelta
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
		"recoveryArchiveFormat": record.ArchiveFormat,
	} {
		if got.Runner[key] != expected {
			t.Fatalf("lost recovery field %s: %v", key, got.Runner[key])
		}
	}
}

// TestRetainedEventRefusesPartiallyBoundRecord keeps this guard exactly as
// strong as it was for the shape it was written about: a HALF-bound record —
// a digest with no bytes, bytes with no digest, or a declared archive format
// with no archive at all — is a half-written publication and is still refused.
//
// What changed with #5370 is that a record declaring NO archive is no longer
// that shape: it is an overflow entry, whose objects live as a pinned mirror
// ref, and journaling it is what authorizes the cleanup that produced it.
// That case is asserted below rather than left implicit.
func TestRetainedEventRefusesPartiallyBoundRecord(t *testing.T) {
	for name, mutate := range map[string]func(*Record){
		"digest without bytes": func(r *Record) { r.ArchiveBytes = 0 },
		"bytes without digest": func(r *Record) { r.ArchiveDigest = "" },
		"format without archive": func(r *Record) {
			r.ArchiveDigest, r.ArchiveBytes, r.ArchiveFormat = "", 0, archiveFormatDelta
		},
	} {
		record := storageTestRecord()
		mutate(&record)
		if _, err := RetainedEvent(record); err == nil {
			t.Fatalf("%s: partially bound snapshot admitted as retained evidence", name)
		}
	}
}

func TestRetainedEventAcceptsAnOverflowRecord(t *testing.T) {
	record := storageTestRecord()
	record.ArchiveDigest, record.ArchiveBytes, record.ArchiveFormat = "", 0, ""
	event, err := RetainedEvent(record)
	if err != nil {
		t.Fatalf("an overflow record could not be journaled: %v", err)
	}
	if event.Runner["recoveryRef"] != record.Ref || event.Runner["recoveryArchiveDigest"] != "" {
		t.Fatalf("overflow observation lost or invented archive identity: %+v", event.Runner)
	}
	// The observation must round-trip: ExplicitlyAbandoned and the selection
	// order both reconstruct a record from exactly these fields.
	observed, err := recordFromEvent(event)
	if err != nil {
		t.Fatalf("overflow observation did not round-trip: %v", err)
	}
	observed.CreatedAt, observed.RetainUntil = record.CreatedAt, record.RetainUntil
	if observed != record {
		t.Fatalf("overflow observation round-tripped to a different record: %+v", observed)
	}
}
