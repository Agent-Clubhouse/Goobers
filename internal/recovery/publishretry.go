package recovery

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
)

// A published archive is immutable evidence, not a cache to regenerate using
// whatever packing heuristics Git currently applies. Verify and re-flush its
// original bytes on retries, including a prior metadata-directory flush error.
func republishRetainedState(ctx context.Context, repository, directory string, prepared, existing Record, maxBytes int64) (Record, error) {
	if !prepared.CreatedAt.Equal(existing.CreatedAt) || !prepared.RetainUntil.Equal(existing.RetainUntil) {
		return Record{}, ErrRecordConflict
	}
	prepared.CreatedAt, prepared.RetainUntil = existing.CreatedAt, existing.RetainUntil
	prepared.ArchiveDigest, prepared.ArchiveBytes = existing.ArchiveDigest, existing.ArchiveBytes
	if prepared != existing {
		return Record{}, ErrRecordConflict
	}
	archive := filepath.Join(directory, BundleFileName)
	if err := ImportSnapshotBundle(ctx, repository, archive, existing, maxBytes); err != nil {
		return Record{}, fmt.Errorf("verify retained archive before retry: %w", err)
	}
	digest, err := publishArchive(ctx, archive, maxBytes, func(w io.Writer) error {
		return copyRecoveryArchive(archive, w)
	})
	if err != nil {
		return Record{}, err
	}
	if digest != existing.ArchiveDigest {
		return Record{}, fmt.Errorf("retained archive changed during retry")
	}
	if err := PublishRecord(filepath.Join(directory, RecordFileName), existing); err != nil {
		return Record{}, err
	}
	return existing, nil
}
