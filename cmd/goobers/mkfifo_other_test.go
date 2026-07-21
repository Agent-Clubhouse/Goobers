//go:build !unix

package main

import "errors"

// mkfifoAsset is unsupported off unix (no FIFOs); the caller treats the error
// as "skip this platform-specific case".
func mkfifoAsset(string) error { return errors.New("mkfifo: unsupported on this platform") }
