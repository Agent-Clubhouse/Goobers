// Package sqlitebusy holds the SQLITE_BUSY/SQLITE_LOCKED retry primitives
// shared by the SQLite-backed stores.
package sqlitebusy

import (
	"errors"

	sqlite "modernc.org/sqlite"
)

// MaxAttempts bounds bounded SQLITE_BUSY retries (migrations, WAL checkpoints,
// intake writes). Sized with real headroom, not the tightest value that
// happened to pass a few runs.
const MaxAttempts = 12

// Is reports whether err (or a wrapped cause) is SQLITE_BUSY (5) or
// SQLITE_LOCKED (6). modernc.org/sqlite's Error.Code() returns the EXTENDED
// result code (e.g. 517 = SQLITE_BUSY_SNAPSHOT = 5 | (2<<8)), so the primary
// code must be masked out with &0xFF before comparing.
func Is(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xFF
	return code == 5 || code == 6
}
