//go:build windows

package secfile

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Well-known SID strings used to set deterministic, owner-independent DACLs via
// icacls, so these tests do not depend on the CI account's identity.
const (
	sidSystem         = "*S-1-5-18"     // NT AUTHORITY\SYSTEM
	sidAdministrators = "*S-1-5-32-544" // BUILTIN\Administrators
	sidEveryone       = "*S-1-1-0"      // Everyone (World)
)

func icacls(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("icacls", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %v: %v\n%s", args, err, out)
	}
}

func writeTemp(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestVerifyPrivate_ToleratesSystemAndAdministrators pins the documented
// tolerance decision: a DACL granting only SYSTEM and Administrators (no other
// trustee) is accepted.
func TestVerifyPrivate_ToleratesSystemAndAdministrators(t *testing.T) {
	path := writeTemp(t)
	icacls(t, path, "/inheritance:r", "/grant:r", sidSystem+":(F)", "/grant:r", sidAdministrators+":(F)")
	if err := VerifyPrivate(path); err != nil {
		t.Errorf("VerifyPrivate(SYSTEM+Administrators only) = %v, want nil", err)
	}
}

// TestVerifyPrivate_AcceptsOwner grants the file solely to its owner (the
// current account) with inheritance stripped, and expects acceptance.
func TestVerifyPrivate_AcceptsOwner(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	path := writeTemp(t)
	icacls(t, path, "/setowner", u.Username)
	icacls(t, path, "/inheritance:r", "/grant:r", u.Username+":(F)")
	if err := VerifyPrivate(path); err != nil {
		t.Errorf("VerifyPrivate(owner-only) = %v, want nil", err)
	}
}

// TestVerifyPrivate_RejectsEveryone proves a world-readable file is rejected —
// the exact case Unix mode bits cannot detect on NTFS.
func TestVerifyPrivate_RejectsEveryone(t *testing.T) {
	path := writeTemp(t)
	icacls(t, path, "/grant:r", sidEveryone+":(R)")
	err := VerifyPrivate(path)
	if err == nil {
		t.Fatal("VerifyPrivate(Everyone:R) = nil, want rejection")
	}
	if !errors.Is(err, ErrNotPrivate) {
		t.Errorf("error not wrapping ErrNotPrivate: %v", err)
	}
	// Contract: Windows remediation is icacls, never chmod.
	if !strings.Contains(err.Error(), "icacls") {
		t.Errorf("VerifyPrivate = %q, want it to contain the icacls remediation", err)
	}
	if strings.Contains(err.Error(), "chmod") {
		t.Errorf("VerifyPrivate = %q, must not suggest chmod on Windows", err)
	}
}

// TestVerifyPrivate_FailsClosedOnMissingFile proves the fail-closed contract on
// Windows: an unreadable security descriptor refuses the secret.
func TestVerifyPrivate_FailsClosedOnMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	err := VerifyPrivate(path)
	if err == nil {
		t.Fatal("VerifyPrivate(missing) = nil, want fail-closed rejection")
	}
	if !errors.Is(err, ErrNotPrivate) {
		t.Errorf("error not wrapping ErrNotPrivate: %v", err)
	}
}
