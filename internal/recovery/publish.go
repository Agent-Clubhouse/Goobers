package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// Retained-state filenames are fixed within a private per-record directory.
const (
	BundleFileName = "snapshot.bundle"
	RecordFileName = "record.json"
)

// PublishRetainedState publishes one prepared snapshot's archive first, then
// its bound metadata. Success means both publications completed durably; an
// error returns no record and never permits cleanup. The caller exclusively
// owns the prepared snapshot and must supply EVERY path cleanup may remove.
// directory must be a pre-existing private per-record directory. Inventory
// allocation/retention belongs to the production coordinator, not this helper.
func PublishRetainedState(ctx context.Context, repository, directory string, cleanupRoots []string, prepared Record, maxBytes int64) (Record, error) {
	if err := prepared.validateSnapshot(); err != nil {
		return Record{}, err
	}
	if err := requireIndependentArchive(directory, append([]string{repository}, cleanupRoots...), len(cleanupRoots) > 0); err != nil {
		return Record{}, err
	}
	handle, err := platformlock.TryAcquire(filepath.Join(directory, ".publish.lock"))
	if err != nil {
		return Record{}, fmt.Errorf("lock retained publication: %w", err)
	}
	defer func() { _ = handle.Release() }()
	archive := filepath.Join(directory, BundleFileName)
	digest, err := PublishSnapshotBundle(ctx, repository, archive, prepared, maxBytes)
	if err != nil {
		return Record{}, err
	}
	info, err := os.Stat(archive)
	if err != nil || !info.Mode().IsRegular() {
		return Record{}, fmt.Errorf("inspect published recovery archive")
	}
	prepared.ArchiveDigest, prepared.ArchiveBytes = digest, info.Size()
	if err := PublishRecord(filepath.Join(directory, RecordFileName), prepared); err != nil {
		return Record{}, fmt.Errorf("publish retained metadata: %w", err)
	}
	return prepared, nil
}

func requireIndependentArchive(directory string, cleanupRoots []string, declared bool) error {
	if !declared {
		return fmt.Errorf("recovery publication requires explicit cleanup roots")
	}
	archive, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	archive, err = filepath.Abs(archive)
	if err != nil {
		return err
	}
	for _, root := range cleanupRoots {
		if root == "" {
			return fmt.Errorf("empty recovery cleanup root")
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return err
		}
		if relative, err := filepath.Rel(resolved, archive); err == nil && filepath.IsLocal(relative) {
			return fmt.Errorf("recovery archive is inside a cleanup root")
		}
	}
	return nil
}
