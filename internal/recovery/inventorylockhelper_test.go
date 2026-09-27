package recovery

import (
	"testing"
	"time"
)

// setInventoryLockWaitForTest shortens the bounded contention wait for a
// single test and restores it afterwards. Tests that deliberately hold the
// inventory lock and assert the guarded operation reports ErrHeld are pinning
// the lock ordering, not the budget, and should not pay for it in wall time.
func setInventoryLockWaitForTest(t *testing.T, wait time.Duration) {
	t.Helper()
	previous := inventoryLockWait
	inventoryLockWait = wait
	t.Cleanup(func() { inventoryLockWait = previous })
}
