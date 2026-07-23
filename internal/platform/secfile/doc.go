// Package secfile verifies that a secret-bearing file is private to its owner,
// portably across Unix and Windows. It is the fourth rung of the
// internal/platform abstraction layer (convention per #620), alongside lock,
// proc, and safeopen.
//
// VerifyPrivate is a security check, so it FAILS CLOSED: if the protection
// state cannot be determined for any reason — a stat error, an unreadable
// security descriptor, a filesystem with no ACL support (FAT32), or any parse
// error — it returns a non-nil error and the caller must refuse the secret.
// "Cannot prove it is private" is treated identically to "it is not private".
// Every rejection wraps [ErrNotPrivate] and carries a platform-appropriate
// remediation hint (chmod on Unix, icacls on Windows) as part of its contract.
//
// # Why mode bits are not portable
//
// On Unix the check is the historical one: the file must not be readable or
// writable by group or other (mode & 0o077 == 0). On Windows, Unix mode bits
// are fiction — Go's os.Stat synthesizes a mode from the read-only attribute
// alone, so a world-readable file happily reports 0600-compatible bits. The
// real protection mechanism on NTFS is the file's DACL, so the Windows
// implementation reads the security descriptor (GetNamedSecurityInfo) and
// inspects the discretionary ACL directly, never the mode.
//
// # Windows tolerance decision: current user, SYSTEM, and Administrators are allowed
//
// The Windows check rejects the file if its DACL grants access to any SID
// other than the file's owner, current user, NT AUTHORITY\SYSTEM, or
// BUILTIN\Administrators.
// SYSTEM and Administrators are deliberately TOLERATED: on a default Windows
// install these principals already have de-facto access to every file (SYSTEM
// runs the kernel; Administrators can take ownership or enable SeBackupPrivilege
// at will), so treating their presence in a DACL as "exposure" would reject
// virtually every real token file while providing no additional protection —
// the secret is already reachable by anyone holding those identities regardless
// of the ACL. This matches how OpenSSH and the GitHub CLI (gh) treat Windows
// key/credential files. A NULL DACL (grants Everyone full access) and any ACE
// granting access to a non-tolerated trustee (Users, Authenticated Users,
// Everyone, or a specific other user) are rejected.
package secfile
