// Package lock provides cross-platform exclusive file locks.
//
// Acquire waits until it can take a lock. TryAcquire never waits and returns
// ErrHeld when another handle owns the lock. A lock remains held until its
// Handle is released or the holding process exits.
//
// Locks are advisory on Unix but mandatory on Windows. Callers must not rely on
// another process being able to read or write a locked file, and lock files must
// reside on a local filesystem.
package lock
