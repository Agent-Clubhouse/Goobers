//go:build windows

package secfile

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// verifyPrivate reads the file's security descriptor and rejects the file
// unless its DACL grants access only to the owner, current user, and tolerated
// system principals SYSTEM and Administrators (see the package doc).
// Unix mode bits are never consulted — on NTFS they are synthesized from the
// read-only attribute and cannot express real access. Fails closed on any
// error reading or parsing the descriptor.
func verifyPrivate(path string) error {
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("%w: %s: cannot read security descriptor "+
			"(the volume may lack ACL support, e.g. FAT32): %w", ErrNotPrivate, path, err)
	}
	// sd is a Go-heap self-relative copy; owner/dacl below are pointers into
	// it. Keep it reachable for the whole ACE walk so the GC cannot free the
	// backing bytes out from under those pointers.
	defer runtime.KeepAlive(sd)

	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("%w: %s: cannot read file owner: %w", ErrNotPrivate, path, err)
	}

	dacl, _, err := sd.DACL()
	// A NULL / not-present DACL is emphatically not "no access" — it grants
	// everyone full control. DACL() reports that as ERROR_OBJECT_NOT_FOUND (or a
	// nil dacl), so treat both as a hard rejection before any generic read error.
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || dacl == nil {
		return fmt.Errorf("%w: %s has no DACL (grants access to everyone); "+
			"run: icacls %q /inheritance:r /grant:r %%USERNAME%%:F", ErrNotPrivate, path, path)
	}
	if err != nil {
		return fmt.Errorf("%w: %s: cannot read DACL: %w", ErrNotPrivate, path, err)
	}

	tolerated, err := toleratedSIDs(owner)
	if err != nil {
		return fmt.Errorf("%w: %s: cannot resolve tolerated system SIDs: %w", ErrNotPrivate, path, err)
	}

	// Only ACCESS_ALLOWED aces can expose the secret; a DENY ace merely
	// restricts. GetAce avoids unsafe pointer arithmetic over the
	// variable-length ACL, which is invalid under the race detector's checkptr.
	for i := uint16(0); i < dacl.AceCount; i++ {
		var allowed *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &allowed); err != nil {
			return fmt.Errorf("%w: %s: cannot read ACE %d: %w", ErrNotPrivate, path, i, err)
		}
		if allowed.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE {
			if allowed.Mask != 0 {
				sid := (*windows.SID)(unsafe.Pointer(&allowed.SidStart))
				if !sidIn(sid, tolerated) {
					return fmt.Errorf("%w: %s grants access to %s beyond its owner; "+
						"run: icacls %q /inheritance:r /grant:r %%USERNAME%%:F",
						ErrNotPrivate, path, sid.String(), path)
				}
			}
		}
	}
	return nil
}

// toleratedSIDs is the set of trustees permitted in a private file's DACL: the
// file's owner and current user plus the always-privileged local principals
// NT AUTHORITY\SYSTEM and BUILTIN\Administrators. Corporate Windows policy can
// make Administrators the owner while granting the creating user an explicit
// ACE, so owner alone does not reliably identify the user who must read it.
func toleratedSIDs(owner *windows.SID) ([]*windows.SID, error) {
	currentUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	return []*windows.SID{owner, currentUser.User.Sid, system, admins}, nil
}

func sidIn(sid *windows.SID, set []*windows.SID) bool {
	for _, s := range set {
		if sid.Equals(s) {
			return true
		}
	}
	return false
}
