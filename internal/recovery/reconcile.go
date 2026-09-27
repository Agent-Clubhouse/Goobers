package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/platform/durability"
)

// IncompleteReservationGrace is how long an incomplete reservation — a
// reservation directory holding no record.json — is treated as a publish that
// may still be in flight rather than as crash debris.
//
// A publish holds its reservation from the moment reserveSnapshotDirectory
// creates the directory until PublishRecord renames record.json into place.
// Between those points it writes one archive, bounded by
// retention.recovery.maxArchiveBytes (default tens of MiB) plus the git
// bundle that produces it, and its own caller's context deadline bounds it
// again. An hour is one to two orders of magnitude more than that work takes
// on the slowest supported storage, so nothing legitimately in flight is
// reclaimed; and it is short enough that a crashed publish stops holding a
// slot within one retention sweep interval rather than for the 30-day retain
// floor that governs published records. An incomplete reservation carries no
// record and therefore no identity, so even when it holds a bundle the bytes
// cannot be attributed to a run, a ref or a repository: there is nothing to
// restore from and nothing to lose by reclaiming it (#5177, #5354).
const IncompleteReservationGrace = time.Hour

// ReconcileResult reports one incomplete reservation and what was done with
// it. A result with Stale false is a publish that may still be in flight: it
// was left untouched and still counts toward maxSnapshots.
type ReconcileResult struct {
	Path    string
	Age     time.Duration
	Stale   bool
	Deleted bool
	DryRun  bool
	Err     error
}

// ReconcileIncompleteReservations reclaims reservation directories that hold
// no record.json — the debris a crashed or killed publish leaves behind,
// typically containing only `.publish.lock` and `snapshot.bundle.lock`. Such a
// directory counts toward maxSnapshots forever and makes the strict inventory
// read fail closed, so without this nothing an operator does short of
// deleting files by hand un-wedges an instance whose inventory has filled
// with it (#5177).
//
// A reservation younger than staleAfter is an identity-matching publish that
// may still be resuming into it, so it is reported and left alone. A
// reservation holding a record.json is never touched here whatever its state;
// deciding that is retirement's and the retention sweep's job. A directory
// holding anything other than the known reservation files, or whose name is
// not a reservation identity, is left alone too: only Goobers' own debris is
// reclaimable, and unrecognised content is evidence until an operator says
// otherwise.
//
// deleteFiles false reports candidates without removing anything, so the
// retention pass's dry-run and first-enable grace window report exactly what
// enforcement would reclaim.
func ReconcileIncompleteReservations(ctx context.Context, root string, limit int, staleAfter time.Duration, deleteFiles bool) ([]ReconcileResult, error) {
	if limit <= 0 || limit > MaxInventoryEntries {
		return nil, fmt.Errorf("invalid recovery inventory reconcile limit")
	}
	if staleAfter <= 0 {
		return nil, fmt.Errorf("invalid recovery reservation stale window")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !before.IsDir() {
		return nil, fmt.Errorf("recovery inventory must be a real directory")
	}
	handle, err := acquireInventoryLock(ctx, root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Release() }()
	names, err := listReservationNames(root, before)
	if err != nil {
		return nil, err
	}
	return reconcileReservations(ctx, root, names, staleAfter, deleteFiles)
}

func reconcileReservations(ctx context.Context, root string, names []string, staleAfter time.Duration, deleteFiles bool) ([]ReconcileResult, error) {
	now := time.Now()
	var results []ReconcileResult
	var failures error
	for _, name := range names {
		if !isReservationName(name) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return results, errors.Join(failures, err)
		}
		directory := filepath.Join(root, name)
		age, incomplete, err := inspectIncompleteReservation(directory, now)
		if err == nil && !incomplete {
			continue
		}
		result := ReconcileResult{Path: directory, Age: age, Stale: age >= staleAfter, DryRun: !deleteFiles, Err: err}
		if result.Err == nil && result.Stale && deleteFiles {
			result.Err = removeIncompleteReservation(ctx, root, directory)
			result.Deleted = result.Err == nil
		}
		if result.Err != nil {
			failures = errors.Join(failures, fmt.Errorf("reconcile incomplete recovery reservation %s: %w", name, result.Err))
		}
		results = append(results, result)
	}
	return results, failures
}

// listReservationNames enumerates root without the cap check
// readInventoryNames applies. An inventory holding MORE entries than the
// configured cap is exactly the wedged state this reconciler exists to clear
// ("130 of 128 slots used", where the read itself refuses), so refusing to
// enumerate it would make reclamation impossible precisely when it is needed.
// The scan is still bounded, by the structural ceiling every writer enforces.
func listReservationNames(root string, before os.FileInfo) ([]string, error) {
	file, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, fmt.Errorf("recovery inventory changed while opening")
	}
	names, err := file.Readdirnames(MaxInventoryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

// isReservationName accepts only the deterministic identity digest
// inventoryDirectoryName produces. Retired entries (ReapRetired's) and
// anything an operator placed in the inventory by hand are excluded by
// construction rather than by a list of exceptions.
func isReservationName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// inspectIncompleteReservation reports a reservation's age and whether it is
// incomplete. Age is the newest modification time in the directory, including
// the directory itself, so a publish that is still writing its archive keeps
// looking young.
func inspectIncompleteReservation(directory string, now time.Time) (time.Duration, bool, error) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return 0, false, nil
	}
	file, err := os.Open(directory)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = file.Close() }()
	contents, err := file.Readdir(len(retiredFileNames()) + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, false, err
	}
	newest := info.ModTime()
	for _, entry := range contents {
		if entry.Name() == RecordFileName || !slices.Contains(retiredFileNames(), entry.Name()) {
			return 0, false, nil
		}
		if entry.ModTime().After(newest) {
			newest = entry.ModTime()
		}
	}
	return now.Sub(newest), true, nil
}

// Caller holds the inventory lock. Remove only the known reservation files,
// never recurse and never follow a link, exactly as the retired-entry reaper
// does. A lock file still held by a live publish fails to be removed on the
// platforms that enforce that, which reports the reservation rather than
// racing it.
func removeIncompleteReservation(ctx context.Context, root, directory string) error {
	for _, file := range retiredFileNames() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := durability.RemoveFile(filepath.Join(directory, file)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(directory); err != nil {
		return err
	}
	return durability.SyncDir(root)
}
