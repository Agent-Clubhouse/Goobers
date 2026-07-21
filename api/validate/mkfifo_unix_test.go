//go:build unix

package validate

import "golang.org/x/sys/unix"

// mkfifoAsset creates a FIFO (a non-regular file) used to prove asset
// validation rejects unsafe entries. FIFOs are a unix concept; the windows
// build gets the stub in mkfifo_other_test.go, and the caller skips.
func mkfifoAsset(path string) error { return unix.Mkfifo(path, 0o600) }
