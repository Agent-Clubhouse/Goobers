package recovery

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type overflowRenewJournal struct{ events []journal.Event }

func (j *overflowRenewJournal) Append(event journal.Event) error {
	j.events = append(j.events, event)
	return nil
}

func TestReadOverflowForRunOnAbsentRootIsEmpty(t *testing.T) {
	entries, err := ReadOverflowForRun(t.Context(), filepath.Join(t.TempDir(), "missing"), "run-1")
	if err != nil {
		t.Fatalf("ReadOverflowForRun: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want none", entries)
	}
}

// With no repository holding the pin, renewal is a journaled skip rather
// than an error: the record keeps its capture-time deadline.
func TestRenewOverflowInRepositoriesWithoutAPinIsASkip(t *testing.T) {
	renewed, skipped, err := RenewOverflowInRepositories(t.Context(), nil, InventoryEntry{}, time.Now())
	if err != nil || renewed != nil || skipped != OverflowSkipNoSnapshotRef {
		t.Fatalf("RenewOverflowInRepositories = (%v, %q, %v), want (nil, %q, nil)", renewed, skipped, err, OverflowSkipNoSnapshotRef)
	}
}

func TestRenewOverflowEntriesSkipsAndJournals(t *testing.T) {
	deadline := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	entries := []InventoryEntry{
		{Record: Record{RunID: "run-1", Ref: "refs/goobers/recovery/late", RetainUntil: deadline}},
		{Record: Record{RunID: "run-1", Ref: "refs/goobers/recovery/skipped", RetainUntil: deadline.Add(-time.Hour)}},
		{Record: Record{RunID: "run-1", Ref: "refs/goobers/recovery/promoted", RetainUntil: deadline.Add(-time.Hour)}},
	}
	var asked []string
	log := &overflowRenewJournal{}
	err := RenewOverflowEntries(entries, deadline, func(entry InventoryEntry) (*Record, string, error) {
		asked = append(asked, entry.Record.Ref)
		if strings.HasSuffix(entry.Record.Ref, "promoted") {
			return nil, "", nil
		}
		return nil, "no managed repository exists", nil
	}, log)
	if err != nil {
		t.Fatalf("RenewOverflowEntries: %v", err)
	}
	if want := []string{"refs/goobers/recovery/skipped", "refs/goobers/recovery/promoted"}; strings.Join(asked, ",") != strings.Join(want, ",") {
		t.Fatalf("renewer asked for %v, want %v (an entry already at the deadline is not renewed)", asked, want)
	}
	if len(log.events) != 1 {
		t.Fatalf("journaled %d events, want exactly the one skip", len(log.events))
	}
	detail := log.events[0].Error
	if log.events[0].Type != journal.EventError || detail == nil || detail.Code != OverflowRenewalSkippedCode {
		t.Fatalf("event = %+v, want a %s error", log.events[0], OverflowRenewalSkippedCode)
	}
	if want := "recovery overflow record for run run-1 ref refs/goobers/recovery/skipped not renewed at terminal finalization: no managed repository exists"; detail.Message != want {
		t.Fatalf("message = %q, want %q", detail.Message, want)
	}
}

func TestRenewOverflowEntriesReturnsARenewalFailure(t *testing.T) {
	boom := errors.New("write failed")
	entries := []InventoryEntry{{Record: Record{RunID: "run-1"}}}
	err := RenewOverflowEntries(entries, time.Now(), func(InventoryEntry) (*Record, string, error) {
		return nil, "", boom
	}, &overflowRenewJournal{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestJournalOverflowRenewalSkippedWithoutARefNamesTheRun(t *testing.T) {
	log := &overflowRenewJournal{}
	JournalOverflowRenewalSkipped(log, "run-1", "", "overflow tier unreadable: denied")
	if len(log.events) != 1 || log.events[0].Error == nil {
		t.Fatalf("events = %+v, want one error event", log.events)
	}
	if want := "recovery overflow record for run run-1 not renewed at terminal finalization: overflow tier unreadable: denied"; log.events[0].Error.Message != want {
		t.Fatalf("message = %q, want %q", log.events[0].Error.Message, want)
	}
}

func TestPromotedOverflowBundleOnAnEmptyInventoryIsAConflict(t *testing.T) {
	_, err := PromotedOverflowBundle(t.Context(), t.TempDir(), Record{RunID: "run-1"})
	if !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("err = %v, want ErrRecordConflict", err)
	}
}
