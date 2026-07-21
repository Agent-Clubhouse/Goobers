package secfile

import "errors"

// ErrNotPrivate is wrapped by every VerifyPrivate rejection — whether the file
// is genuinely exposed or its protection state could not be determined
// (fail-closed). Callers can test for it with errors.Is; the wrapped message
// carries the specific reason plus a platform-appropriate remediation hint.
var ErrNotPrivate = errors.New("secret file is not private to its owner")

// VerifyPrivate returns nil only if the file at path is provably private to its
// owner: not group/other-accessible on Unix, or granting DACL access to no
// trustee beyond the owner and tolerated system principals on Windows (see the
// package doc). It fails closed — any error determining the state (missing
// file, unreadable ACL, unsupported filesystem) yields a non-nil error wrapping
// ErrNotPrivate. It never mutates the file.
func VerifyPrivate(path string) error {
	return verifyPrivate(path)
}
