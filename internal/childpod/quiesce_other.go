//go:build !linux

package childpod

import (
	"context"
	"fmt"
)

// VerifyEntrypoint refuses platforms without the supported container boundary.
func VerifyEntrypoint() error { return fmt.Errorf("isolated child execution requires a Linux pod") }

// Quiesce never asserts writer termination on an unsupported platform.
func Quiesce(context.Context) error { return VerifyEntrypoint() }
