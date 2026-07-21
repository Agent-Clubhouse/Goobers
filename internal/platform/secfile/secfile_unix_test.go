//go:build unix

package secfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyPrivate_AcceptsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 0600 and tighter (0400) must be accepted.
	for _, mode := range []os.FileMode{0o600, 0o400, 0o000} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := VerifyPrivate(path); err != nil {
			t.Errorf("VerifyPrivate(mode %#o) = %v, want nil", mode, err)
		}
	}
}

func TestVerifyPrivate_RejectsGroupOrOther(t *testing.T) {
	// Every mode with any group/other bit set must be rejected.
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o606, 0o777, 0o601} {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		err := VerifyPrivate(path)
		if err == nil {
			t.Errorf("VerifyPrivate(mode %#o) = nil, want rejection", mode)
			continue
		}
		if !errors.Is(err, ErrNotPrivate) {
			t.Errorf("VerifyPrivate(mode %#o) error not wrapping ErrNotPrivate: %v", mode, err)
		}
		// Contract: the message names the path and gives the chmod remediation.
		for _, want := range []string{path, "chmod 600"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("VerifyPrivate(mode %#o) = %q, want it to contain %q", mode, err, want)
			}
		}
	}
}

// TestVerifyPrivate_FailsClosedOnMissingFile proves the fail-closed contract:
// an indeterminate protection state (here, a stat error) refuses the secret
// rather than trusting it.
func TestVerifyPrivate_FailsClosedOnMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	err := VerifyPrivate(path)
	if err == nil {
		t.Fatal("VerifyPrivate(missing) = nil, want fail-closed rejection")
	}
	if !errors.Is(err, ErrNotPrivate) {
		t.Errorf("VerifyPrivate(missing) error not wrapping ErrNotPrivate: %v", err)
	}
}
