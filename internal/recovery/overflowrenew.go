package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// OverflowRenewalSkippedCode is the journal error code recording why an
// overflow record kept its capture-time deadline at terminal finalization.
const OverflowRenewalSkippedCode = "recovery_overflow_renewal_skipped"

// OverflowSkipNoSnapshotRef is the skip reason for an overflow record whose
// pin no managed repository still resolves.
const OverflowSkipNoSnapshotRef = "no managed repository holds the snapshot ref"

// ReadOverflowForRun finds runID's overflow-tier records under root. Like
// the inventory read for bundle renewal it is tolerant: an entry no scan can
// interpret carries no run identity, so it is not this run's.
func ReadOverflowForRun(ctx context.Context, root, runID string) ([]InventoryEntry, error) {
	entries, _, err := ReadOverflow(ctx, root)
	if err != nil {
		return nil, err
	}
	var matching []InventoryEntry
	for _, entry := range entries {
		if entry.Record.RunID == runID {
			matching = append(matching, entry)
		}
	}
	return matching, nil
}

// OverflowSource picks the repository that still holds record's pin. A
// record may have been captured into the mirror while a pinned clone never
// saw it, so "the first repository" is not good enough.
func OverflowSource(ctx context.Context, repositories []string, record Record) (string, error) {
	for _, repository := range repositories {
		if HasSnapshotRef(ctx, repository, record) {
			return repository, nil
		}
	}
	return "", nil
}

// RenewOverflowInRepositories renews one overflow entry to deadline in
// whichever of repositories still pins it. A nil record with a nil error
// means nothing was renewed; skipped then says why, and is empty when the
// record was promoted or retired meanwhile, which is not a skip (a later
// inventory read renews a promoted bundle).
func RenewOverflowInRepositories(ctx context.Context, repositories []string, entry InventoryEntry, deadline time.Time) (renewed *Record, skipped string, err error) {
	source, _ := OverflowSource(ctx, repositories, entry.Record)
	if source == "" {
		return nil, OverflowSkipNoSnapshotRef, nil
	}
	record, err := RenewOverflowRetention(ctx, source, entry.RecordPath, deadline)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, "", nil
	case errors.Is(err, ErrOverflowRefUnresolved):
		return nil, OverflowSkipNoSnapshotRef, nil
	case err != nil:
		return nil, "", err
	}
	return &record, "", nil
}

// OverflowRenewer renews one overflow entry, returning the renewed record,
// or nil and the reason it was skipped ("" when it is not a skip).
type OverflowRenewer func(entry InventoryEntry) (renewed *Record, skipped string, err error)

// RenewOverflowEntries extends each overflow entry still short of deadline
// through renew (#5403), journaling a renewal under the overflow tier marker
// and a skip as a best-effort explanation. Only a failure to write or journal
// a verified extension is returned: an entry renew skips keeps its
// capture-time deadline, exactly its pre-#5403 behaviour.
func RenewOverflowEntries(entries []InventoryEntry, deadline time.Time, renew OverflowRenewer, log PublicationJournal) error {
	for _, entry := range entries {
		if !entry.Record.RetainUntil.Before(deadline) {
			continue
		}
		renewed, skipped, err := renew(entry)
		if err != nil {
			return err
		}
		if renewed == nil {
			if skipped != "" {
				JournalOverflowRenewalSkipped(log, entry.Record.RunID, entry.Record.Ref, skipped)
			}
			continue
		}
		event, err := RetainedEvent(*renewed)
		if err != nil {
			return err
		}
		// The same tier marker the capture acknowledgement carries, so the
		// renewal is never mistaken for bundle-durable work.
		event.Runner["recoveryOverflow"] = true
		if err := log.Append(event); err != nil {
			return err
		}
	}
	return nil
}

// JournalOverflowRenewalSkipped records why an overflow record kept its
// capture-time deadline at terminal finalization. Best-effort by
// construction: the explanation must not become the reason finalization is
// deferred.
func JournalOverflowRenewalSkipped(log PublicationJournal, runID, ref, reason string) {
	subject := "run " + runID
	if ref != "" {
		subject += " ref " + ref
	}
	message := fmt.Sprintf("recovery overflow record for %s not renewed at terminal finalization: %s", subject, reason)
	_ = log.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: OverflowRenewalSkippedCode, Message: message},
	})
}

// CarryOverflowDeadline makes the promoted bundle's effective deadline at
// least the overflow record's (#5403). It re-reads the overflow record after
// the bundle is published rather than trusting the copy promotion started
// from: a terminal renewal that moved it in between must not be lost when the
// overflow record is deleted. Extension is forward-only, so a bundle that
// already carries a later deadline is left as it is.
func CarryOverflowDeadline(ctx context.Context, overflowPath, bundlePath string, deadline time.Time, maxArchiveBytes int64) error {
	if current, err := ReadOverflowRecord(overflowPath); err == nil && current.RetainUntil.After(deadline) {
		deadline = current.RetainUntil
	}
	published, err := ReadRetainedRecord(bundlePath)
	if err != nil {
		return err
	}
	if !deadline.After(published.RetainUntil) {
		return nil
	}
	_, err = RenewRetention(ctx, bundlePath, deadline, maxArchiveBytes)
	return err
}

// PromotedOverflowBundle finds the bundle an interrupted promotion already
// published for record under the inventory root: the same identity in every
// field but the deadline, which is the one field a renewal may have moved
// since. ErrRecordConflict means no such bundle exists.
func PromotedOverflowBundle(ctx context.Context, root string, record Record) (string, error) {
	entries, _, err := ReadInventoryTolerant(ctx, root, MaxInventoryEntries)
	if err != nil {
		return "", err
	}
	want := record
	want.RetainUntil = time.Time{}
	for _, entry := range entries {
		got := entry.Record
		got.RetainUntil = time.Time{}
		got.ArchiveDigest, got.ArchiveBytes, got.ArchiveFormat = "", 0, ""
		if got == want {
			return entry.RecordPath, nil
		}
	}
	return "", ErrRecordConflict
}
