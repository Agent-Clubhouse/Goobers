package recovery

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// AcceptArchive accepts a worker archive into host custody. The caller must
// authenticate run/repository ownership, exclusively own the receiving Git
// repository, and revalidate its authorization in acknowledge before returning
// success. The returned record describes the host's verified archive (Git may
// repack the worker's objects). Neither a blob write nor a Git import is an ACK.
// Transport wiring and policy (including deadlines) belong to the caller.
func AcceptArchive(ctx context.Context, source io.Reader, request RetentionRequest, acknowledge PublicationJournal) (Record, string, error) {
	if source == nil || acknowledge == nil || request.MaxSnapshots <= 0 || request.MaxSnapshots > 10000 || request.MaxArchiveBytes <= 0 {
		return Record{}, "", fmt.Errorf("archive intake requires bounded durable custody")
	}
	if _, err := RefForRun(request.RunID); err != nil {
		return Record{}, "", err
	}
	if !validRepositoryKey(request.RepositoryKey) || request.IdentityTime.IsZero() || !request.RetainUntil.After(request.IdentityTime) {
		return Record{}, "", fmt.Errorf("archive intake requires authoritative identity and retention")
	}
	if err := requireIndependentArchive(request.InventoryRoot, append([]string{request.Repository}, request.CleanupRoots...), len(request.CleanupRoots) > 0); err != nil {
		return Record{}, "", err
	}
	directory, err := os.MkdirTemp("", "goobers-recovery-intake-*")
	if err != nil {
		return Record{}, "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	record, err := receiveArchiveEnvelope(ctx, source, directory, request.MaxArchiveBytes, func(record Record) error {
		if record.RunID != request.RunID || record.RepositoryKey != request.RepositoryKey || !record.CreatedAt.Equal(request.IdentityTime) || !record.RetainUntil.Equal(request.RetainUntil) {
			return fmt.Errorf("archive intake identity or retention mismatch")
		}
		return nil
	})
	if err != nil {
		return Record{}, "", err
	}
	// Reserve capacity under the inventory lock before importing objects or
	// creating a host ref. Failed imports leave a bounded retry reservation.
	retained, path, err := publishToInventory(ctx, request.Repository, request.InventoryRoot, request.CleanupRoots, record, request.MaxSnapshots, request.MaxArchiveBytes, func() error {
		return ImportSnapshotBundle(ctx, request.Repository, filepath.Join(directory, BundleFileName), record, request.MaxArchiveBytes)
	})
	if err != nil {
		return Record{}, "", err
	}
	event, err := RetainedEvent(retained)
	if err != nil {
		return Record{}, "", err
	}
	event.Runner["recoveryCapture"] = true
	if err := ctx.Err(); err != nil {
		return Record{}, "", err
	}
	if err := acknowledge.Append(event); err != nil {
		return Record{}, "", fmt.Errorf("acknowledge received recovery archive: %w", err)
	}
	return retained, path, nil
}
