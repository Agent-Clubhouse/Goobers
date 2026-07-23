package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// ReserveTerminalForPrune prevents a terminal journal from being resumed while
// telemetry pruning moves and removes it. A live or paused run is not reserved.
func ReserveTerminalForPrune(dir string) (bool, error) {
	lock, err := platformlock.TryAcquire(filepath.Join(dir, fileLock))
	if errors.Is(err, platformlock.ErrHeld) {
		return false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("journal: reserve run for telemetry pruning: %w", err)
	}
	defer func() { _ = lock.Release() }()

	reader, err := OpenRead(dir)
	if err != nil {
		return false, err
	}
	phase, err := reader.Phase()
	if err != nil {
		return false, err
	}
	if phase == PhaseRunning {
		return false, nil
	}
	if err := WriteFileAtomic(filepath.Join(dir, filePruning), []byte("reserved\n"), 0o644); err != nil {
		return false, fmt.Errorf("journal: write telemetry pruning reservation: %w", err)
	}
	return true, nil
}

// ClearPruneReservation removes a reservation after a prune rollback.
func ClearPruneReservation(dir string) error {
	if err := os.Remove(filepath.Join(dir, filePruning)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("journal: clear telemetry pruning reservation: %w", err)
	}
	return nil
}
