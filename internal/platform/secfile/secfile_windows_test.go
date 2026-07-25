//go:build windows

package secfile

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
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

func setDACLFromSDDL(t *testing.T, path, dacl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(dacl)
	if err != nil {
		t.Fatalf("parse SDDL %q: %v", dacl, err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("read DACL from SDDL %q: %v", dacl, err)
	}
	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
	runtime.KeepAlive(sd)
	if err != nil {
		t.Fatalf("set DACL from SDDL %q: %v", dacl, err)
	}
}

func currentUserSID(t *testing.T) string {
	t.Helper()
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("get current user SID: %v", err)
	}
	return current.User.Sid.String()
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

func TestVerifyPrivate_AcceptsSimpleDenyACE(t *testing.T) {
	path := writeTemp(t)
	setDACLFromSDDL(t, path, fmt.Sprintf(
		"D:P(D;;0x1;;;WD)(A;;FA;;;%s)",
		currentUserSID(t),
	))
	if err := VerifyPrivate(path); err != nil {
		t.Errorf("VerifyPrivate(simple deny ACE) = %v, want nil", err)
	}
}

func TestVerifyPrivate_RejectsNonSimpleAllowACEs(t *testing.T) {
	const objectGUID = "00112233-4455-6677-8899-aabbccddeeff"
	tests := []struct {
		name    string
		aceType byte
		ace     string
	}{
		{
			name:    "object",
			aceType: 5,
			ace:     "(OA;;FR;" + objectGUID + ";;WD)",
		},
		{
			name:    "callback",
			aceType: 9,
			ace:     `(XA;;FR;;;WD;(@User.Title=="PM"))`,
		},
		{
			name:    "callback object",
			aceType: 11,
			ace:     `(ZA;;FR;` + objectGUID + `;;WD;(@User.Title=="PM"))`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t)
			setDACLFromSDDL(t, path, fmt.Sprintf(
				"D:P(A;;FA;;;%s)%s",
				currentUserSID(t),
				tt.ace,
			))

			err := VerifyPrivate(path)
			if err == nil {
				t.Fatalf("VerifyPrivate(ACE type %d) = nil, want fail-closed rejection", tt.aceType)
			}
			if !errors.Is(err, ErrNotPrivate) {
				t.Errorf("error not wrapping ErrNotPrivate: %v", err)
			}
			if want := fmt.Sprintf("unsupported ACE type %d", tt.aceType); !strings.Contains(err.Error(), want) {
				t.Errorf("VerifyPrivate = %q, want it to contain %q", err, want)
			}
			if !strings.Contains(err.Error(), "cannot verify privacy") {
				t.Errorf("VerifyPrivate = %q, want fail-closed explanation", err)
			}
			if !strings.Contains(err.Error(), "icacls") {
				t.Errorf("VerifyPrivate = %q, want it to contain the icacls remediation", err)
			}
		})
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
