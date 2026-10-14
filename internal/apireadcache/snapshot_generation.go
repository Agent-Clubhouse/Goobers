package apireadcache

import "github.com/goobers/goobers/internal/apireadstore"

// snapshotReadKey captures the shared generation before a provider read starts.
// Invalidation rotates that generation atomically. Late fills remain unreachable
// to subsequent reads, even when the same wrapper or another process makes them.
func (c *apiReadCache) snapshotReadKey(key string) (string, error) {
	var generation string
	err := c.withDisk(func(store *apireadstore.Store) error {
		var err error
		generation, err = store.SnapshotGeneration(c.snapshotID)
		return err
	})
	if err != nil {
		return "", err
	}
	return apiReadSnapshotKey(c.snapshotID+"\x00"+generation, key), nil
}
