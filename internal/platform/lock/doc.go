// Package lock provides cross-platform exclusive file locks.
//
// Acquire waits until it can take a lock. TryAcquire never waits and returns
// ErrHeld when another handle owns the lock. A lock remains held until its
// Handle is released or the holding process exits.
//
// Locks are advisory on Unix. Windows mandatorily locks a reserved byte at a
// high offset, leaving metadata at the start of the file accessible to other
// processes. Lock files must reside on a local filesystem.
package lock
