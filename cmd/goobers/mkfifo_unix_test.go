//go:build unix

package main

import "golang.org/x/sys/unix"

// mkfifoAsset creates a FIFO (a non-regular file) used to prove the config
// digest rejects unsafe assets. FIFOs are unix-only; the windows build uses the
// stub in mkfifo_other_test.go and the caller skips.
func mkfifoAsset(path string) error { return unix.Mkfifo(path, 0o600) }
