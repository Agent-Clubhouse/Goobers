package recovery

import (
	"context"
	"fmt"
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
	SkipEmpty bool
}

// PublicationJournal is the durable acknowledgement boundary. Production uses
// an instance journal rather than reopening the active run's writer.
type PublicationJournal interface {
	Append(journal.Event) error
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
	if request.SkipEmpty && prepared.PatchDigest == "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		return Record{}, "", nil
	}
	retained, path, err := PublishToInventory(ctx, request.Repository, request.InventoryRoot, request.CleanupRoots, prepared, request.MaxSnapshots, request.MaxArchiveBytes)
	if err != nil {
		return Record{}, "", err
	}
	event, err := RetainedEvent(retained)
	if err != nil {
		return Record{}, "", err
	}
	if err := ctx.Err(); err != nil {
		return Record{}, "", err
	}
	if err := log.Append(event); err != nil {
		return Record{}, "", fmt.Errorf("journal recovery publication: %w", err)
	}
	return retained, path, nil
}
