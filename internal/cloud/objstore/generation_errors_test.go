package objstore

import (
	"context"
	"errors"
	"io/fs"
	"syscall"
	"testing"
)

// A full temp volume or a cancelled request must not permanently refuse a
// valid generation.
func TestMaterializeFailuresAreTransientNotInvalid(t *testing.T) {
	for name, cause := range map[string]error{
		"volume full":   &fs.PathError{Op: "write", Path: "config/x", Err: syscall.ENOSPC},
		"cancelled":     context.Canceled,
		"deadline":      context.DeadlineExceeded,
		"no permission": &fs.PathError{Op: "mkdir", Path: "config", Err: fs.ErrPermission},
	} {
		t.Run(name, func(t *testing.T) {
			err := materializeError("extract archive", cause)
			if !errors.Is(err, ErrGenerationUnavailable) || errors.Is(err, ErrGenerationInvalid) || !errors.Is(err, cause) {
				t.Fatalf("materializeError(%v) = %v, want transient and keeping its cause", cause, err)
			}
		})
	}
}
