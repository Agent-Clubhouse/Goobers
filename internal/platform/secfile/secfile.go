package secfile

import (
	"errors"
	"os"
)

// ErrNotPrivate is wrapped by every VerifyPrivate rejection — whether the file
// is genuinely exposed or its protection state could not be determined
// (fail-closed). Callers can test for it with errors.Is; the wrapped message
// carries the specific reason plus a platform-appropriate remediation hint.
var ErrNotPrivate = errors.New("secret file is not private to its owner")

// VerifyPrivate returns nil only if the file at path is provably protected:
// owner-only or on a read-only tmpfs on Unix, or granting DACL access to no
// trustee beyond the owner and tolerated system principals on Windows (see the
// package doc). It fails closed — any error determining the state (missing
// file, unreadable ACL, unsupported filesystem) yields a non-nil error wrapping
// ErrNotPrivate. It never mutates the file.
func VerifyPrivate(path string) error {
	return verifyPrivate(path)
}

// WritePrivate writes data to path, creating or truncating it, so that the file
// is private to the current user before any of data reaches it, then proves the
// result with VerifyPrivate (so it fails closed exactly where VerifyPrivate
// does). On Unix the file is opened 0600 and narrowed to 0600 before the write,
// since the create mode is ignored for a file that already exists. On Windows,
// where mode bits restrict nothing, the file is created with — and then
// explicitly given — a protected DACL granting only the current user, SYSTEM
// and Administrators (the set VerifyPrivate tolerates), so it never relies on
// the DACL it would inherit from its parent directory.
//
// It fails closed: once the file has been opened, any later failure (writing,
// narrowing, or the final verification) removes it rather than leave a
// partial or unverified secret on disk.
func WritePrivate(path string, data []byte) error {
	opened, err := writePrivate(path, data)
	if err == nil {
		err = VerifyPrivate(path)
	}
	if err != nil && opened {
		_ = os.Remove(path)
	}
	return err
}
