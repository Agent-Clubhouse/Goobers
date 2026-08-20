// Package lock provides cross-platform exclusive file locks.
//
// TryAcquire never waits and returns ErrHeld when another handle owns the
// lock. There is no blocking Acquire: every caller retries TryAcquire against
// a bounded deadline instead, so a stale holder cannot wedge a caller forever
// (#2905). A lock remains held until its Handle is released or the holding
// process exits.
//
// Locks are advisory on Unix. Windows mandatorily locks a reserved byte at a
// high offset, leaving metadata at the start of the file accessible to other
// processes. Lock files must reside on a local filesystem.
package lock
