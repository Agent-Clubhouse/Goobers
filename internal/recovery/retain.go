package recovery

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// RetentionRequest carries caller-verified ownership, storage policy, and the
// stable timestamps used across retries. InventoryRoot must already be private
// and durable; CleanupRoots must enumerate every deletable source location.
type RetentionRequest struct {
	Repository      string
	RepositoryKey   string
	RunID           string
	BaseRef         string
	IdentityTime    time.Time
	RetainUntil     time.Time
	InventoryRoot   string
	CleanupRoots    []string
	MaxSnapshots    int
	MaxArchiveBytes int64
	// SkipEmpty permits cleanup without publishing when no implementation
	// differs from the cumulative base. It returns a zero record and empty path.
	// RetainAbandonedPreparation applies it to a preparation that differs in
	// nothing from its parent, which would otherwise hold a scarce inventory
	// slot for the full retain floor while protecting no work at all (#4994).
	SkipEmpty bool
	// AcknowledgeArchive optionally requires custody outside the local
	// inventory before releasing source state. The path is the bundle, not
	// its metadata sidecar. Failure preserves the source for an identical retry.
	AcknowledgeArchive func(context.Context, Record, string) error
	// EvictFull optionally retires a reclaimable entry when the inventory is
	// full, so this capture is not refused when reclaimable capacity exists
	// (#4823). Nil disables eviction; a full inventory then still refuses.
	EvictFull EvictFunc
	// OverflowRoot enables the overflow tier (#5370): when the inventory is
	// still full after every reclamation has run, the snapshot is published
	// here as a ref-backed record with no bundle instead of refusing the
	// cleanup. Empty keeps the historical fail-closed behaviour, which is
	// what retention.recovery.onFull: refuse selects.
	OverflowRoot string
}

// PublicationJournal is the durable acknowledgement boundary. Production uses
// an instance journal rather than reopening the active run's writer.
type PublicationJournal interface {
	Append(journal.Event) error
}

func (r RetentionRequest) acknowledgeArchive(ctx context.Context, record Record, recordPath string) error {
	if r.AcknowledgeArchive == nil {
		return nil
	}
	return r.AcknowledgeArchive(ctx, record, filepath.Join(filepath.Dir(recordPath), BundleFileName))
}

// Retain captures, publishes, and journals one snapshot before acknowledging
// cleanup. Every failure returns a zero record/path: callers must preserve the
// source even if a partial operation already left a recoverable archive. An
// identical retry reuses the reservation and may append a duplicate observation
// of the same immutable snapshot; consumers must key state by its recovery ref.
func Retain(ctx context.Context, request RetentionRequest, log PublicationJournal) (Record, string, error) {
	if log == nil {
		return Record{}, "", fmt.Errorf("recovery requires a durable publication journal")
	}
	prepared, err := PrepareRecord(ctx, request.Repository, request.RepositoryKey, request.RunID, request.BaseRef, request.IdentityTime, request.RetainUntil)
	if err != nil {
		return Record{}, "", err
	}
	if request.SkipEmpty && prepared.PatchDigest == emptyPatchDigest {
		return Record{}, "", nil
	}
	retained, path, err := PublishToInventoryWithEviction(ctx, request.Repository, request.InventoryRoot, request.CleanupRoots, prepared, request.MaxSnapshots, request.MaxArchiveBytes, request.EvictFull)
	overflowed := false
	if errors.Is(err, ErrInventoryFull) && request.OverflowRoot != "" {
		retained, path, err = PublishOverflow(ctx, request.Repository, request.OverflowRoot, prepared)
		overflowed = err == nil
	}
	if err != nil {
		return Record{}, "", err
	}
	event, err := RetainedEvent(retained)
	if err != nil {
		return Record{}, "", err
	}
	if overflowed {
		// The tier is part of the durable acknowledgement, not a log line: a
		// reader deciding whether this work is bundle-durable or ref-durable
		// has no other way to tell, and promotion is what clears it.
		event.Runner["recoveryOverflow"] = true
	}
	// Only actual source capture establishes ordering. Inventory readbacks
	// and retention renewals also emit custody metadata, but must not make
	// an older snapshot look like the current implementation.
	event.Runner["recoveryCapture"] = true
	if err := ctx.Err(); err != nil {
		return Record{}, "", err
	}
	if err := log.Append(event); err != nil {
		return Record{}, "", fmt.Errorf("journal recovery publication: %w", err)
	}
	// An overflow entry has no archive to hand to external custody. Skipping
	// the hook is not weaker custody than before: the alternative was no
	// record at all and a refused cleanup.
	if !overflowed {
		if err := request.acknowledgeArchive(ctx, retained, path); err != nil {
			return Record{}, "", err
		}
	}
	return retained, path, nil
}
