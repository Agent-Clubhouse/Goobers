package failureclass

import "strings"

// windowsLockMarkers are distinctive substrings that Git, Node, and native
// Windows file operations emit when a process — typically real-time
// antivirus scanning — holds a transient lock on a file Goobers is writing
// or immediately rereading (#4416). Bare "permission denied" / "access is
// denied" are deliberately excluded: those phrases alone are
// indistinguishable from a genuine authorization bug, the same over-broad
// match automated.go's infrastructureSignatures documents avoiding — each
// entry here instead names a lock-specific shape.
var windowsLockMarkers = []string{
	"sharing violation",
	"used by another process",
	"the process cannot access the file",
	"resource busy or locked", // Node's EBUSY message
	"unable to unlink",        // git's unlink failure during checkout/clean
}

// windowsLockErrnoOperations pair with "eperm" so a Node EPERM — which
// otherwise reads exactly like an application permission bug — only counts
// when it names one of the filesystem mutations a transient lock actually
// blocks (Node's well-documented Windows AV-lock shape, e.g. "EPERM:
// operation not permitted, unlink 'C:\\...'"). "open" is deliberately
// omitted: an EPERM on open is common for a genuine authorization denial.
var windowsLockErrnoOperations = []string{"unlink", "rename", "rmdir"}

// IsWindowsSharingViolation reports whether message describes a transient
// Windows file-lock/sharing-violation condition — the kind of host
// contention real-time antivirus scanning causes — rather than a genuine
// permission or logic bug. Matching is case-insensitive.
func IsWindowsSharingViolation(message string) bool {
	message = strings.ToLower(message)
	if containsAny(message, windowsLockMarkers) {
		return true
	}
	return strings.Contains(message, "eperm") && containsAny(message, windowsLockErrnoOperations)
}
