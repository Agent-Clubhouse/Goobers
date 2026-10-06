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
	if source == nil || acknowledge == nil || request.MaxSnapshots <= 0 || request.MaxSnapshots > MaxInventoryEntries || request.MaxArchiveBytes <= 0 {
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
	record, err := ReceiveArchiveEnvelope(ctx, source, directory, request.MaxArchiveBytes, func(record Record) error {
		if record.RunID != request.RunID || record.RepositoryKey != request.RepositoryKey {
			return fmt.Errorf("archive intake identity mismatch")
		}
		return nil
	})
	if err != nil {
		return Record{}, "", err
	}
	return acceptReceivedArchive(ctx, directory, record, request, acknowledge)
}

func acceptReceivedArchive(ctx context.Context, directory string, record Record, request RetentionRequest, acknowledge PublicationJournal) (Record, string, error) {
	custodyCtx := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		custodyCtx, cancel = context.WithDeadline(custodyCtx, deadline)
		defer cancel()
	}
	// Worker clocks and retention wishes are not authoritative. These fields
	// do not affect bundle/patch identity; host policy binds the durable record.
	record.CreatedAt = request.IdentityTime
	record.RetainUntil = request.RetainUntil
	if retained, path, reused, err := acceptExistingCleanArchive(custodyCtx, filepath.Join(directory, BundleFileName), record, request); err != nil {
		return Record{}, "", err
	} else if reused {
		event, err := RetainedEvent(retained)
		if err != nil {
			return Record{}, "", err
		}
		event.Runner["recoveryCapture"] = true
		if err := custodyCtx.Err(); err != nil {
			return Record{}, "", err
		}
		if err := acknowledge.Append(event); err != nil {
			return Record{}, "", fmt.Errorf("acknowledge existing recovery archive: %w", err)
		}
		return retained, path, nil
	}
	// Reserve capacity under the inventory lock before importing objects or
	// creating a host ref. Failed imports leave a bounded retry reservation.
	retained, path, err := publishToInventory(custodyCtx, request.Repository, request.InventoryRoot, request.CleanupRoots, record, request.MaxSnapshots, request.MaxArchiveBytes, func() error {
		if request.EnsureBase != nil && record.archiveFormat() == archiveFormatDelta {
			if !baseUsableForDelta(custodyCtx, request.Repository, record) {
				if err := request.EnsureBase(custodyCtx, request.Repository, record.BaseSHA, record.BaseRef); err != nil {
					return fmt.Errorf("ensure recovery base commit %s: %w", record.BaseSHA, err)
				}
			}
		}
		return ImportSnapshotBundle(custodyCtx, request.Repository, filepath.Join(directory, BundleFileName), record, request.MaxArchiveBytes)
	}, request.EvictFull)
	if err != nil {
		return Record{}, "", err
	}
	event, err := RetainedEvent(retained)
	if err != nil {
		return Record{}, "", err
	}
	event.Runner["recoveryCapture"] = true
	if err := custodyCtx.Err(); err != nil {
		return Record{}, "", err
	}
	if err := acknowledge.Append(event); err != nil {
		return Record{}, "", fmt.Errorf("acknowledge received recovery archive: %w", err)
	}
	return retained, path, nil
}

// baseUsableForDelta reports whether the receiving repository both holds a
// delta's base and can prove it reachable from the record's base ref. Without
// the latter the host re-captures a full bundle of the whole history (#6306).
func baseUsableForDelta(ctx context.Context, repository string, record Record) bool {
	if recoveryGit(ctx, repository, io.Discard, "cat-file", "-e", record.BaseSHA+"^{commit}") != nil {
		return false
	}
	reachable, _ := baseProvablyReachable(ctx, repository, record)
	return reachable || record.BaseRef == ""
}
