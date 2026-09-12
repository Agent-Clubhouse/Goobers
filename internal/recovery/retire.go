package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/platform/durability"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

const retiredPrefix = ".retired-"

// RetireSnapshot commits an already-authorized retention decision. The caller
// must hold the run lifecycle locks and prove terminality and retention policy;
// this helper only protects the exact inventory record against replacement or
// renewal. unpin must remove the exact owned ref without touching user branches.
// Success returns a retired directory whose files may subsequently be reaped.
// Retired directories still count toward inventory capacity until removed.
func RetireSnapshot(ctx context.Context, root string, expected Record, unpin func(Record) error) (string, error) {
	if err := expected.Validate(); err != nil {
		return "", err
	}
	if unpin == nil {
		return "", fmt.Errorf("recovery retirement requires owned-ref cleanup")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("recovery inventory must be a real directory")
	}
	lock, err := platformlock.TryAcquire(filepath.Join(root, ".inventory.lock"))
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Release() }()
	name := inventoryDirectoryName(expected)
	directory := filepath.Join(root, name)
	retired := filepath.Join(root, retiredPrefix+name)
	// Never overwrite an earlier retirement, including an empty directory
	// left by interrupted file cleanup. Reconciliation handles those first.
	if _, err := os.Lstat(retired); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("recovery retirement requires reconciliation")
	}
	if _, err := readInventoryEntry(root, name); err != nil {
		return "", err
	}
	publication, err := platformlock.TryAcquire(filepath.Join(directory, ".publish.lock"))
	if err != nil {
		return "", err
	}
	defer func() { _ = publication.Release() }()
	current, err := ReadRetainedRecord(filepath.Join(directory, RecordFileName))
	if err != nil {
		return "", err
	}
	if current != expected {
		return "", ErrRecordConflict
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := unpin(current); err != nil {
		return "", err
	}
	// On Windows the open lock file prevents moving its directory. Inventory
	// ownership still excludes publishers and renewers after releasing it.
	if err := publication.Release(); err != nil {
		return "", err
	}
	if err := durability.Move(directory, retired); err != nil {
		return "", err
	}
	if err := durability.SyncDir(root); err != nil {
		return "", err
	}
	return retired, nil
}

func isRetiredName(name string) bool {
	if !strings.HasPrefix(name, retiredPrefix) {
		return false
	}
	digest := strings.TrimPrefix(name, retiredPrefix)
	return len(digest) == 64 && gitObjectID.MatchString(digest)
}

// Validate the bounded, non-recursive retirement namespace even after some
// files have been removed. Unknown files are never hidden from inventory reads.
func validateRetiredDirectory(root, name string) error {
	if !isRetiredName(name) {
		return ErrRecordConflict
	}
	directory := filepath.Join(root, name)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("retired recovery reservation must be a real directory")
	}
	if err := validateReservationContents(directory); err != nil {
		return err
	}
	record, err := ReadRecord(filepath.Join(directory, RecordFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if name != retiredPrefix+inventoryDirectoryName(record) {
		return ErrRecordConflict
	}
	return nil
}
