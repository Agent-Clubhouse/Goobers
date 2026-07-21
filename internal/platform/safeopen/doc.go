// Package safeopen provides cross-platform, symlink-safe read-only opens.
//
// It exists so security-sensitive call sites (e.g. internal/gooberassets, which
// materializes a goober's asset bundle from an untrusted-content directory)
// never reach for platform-specific syscalls or branch on runtime.GOOS
// directly. Following the convention of internal/platform/lock and
// internal/platform/proc, the surface here is small and the behavior lives in
// build-tagged files:
//
//   - Open opens a path read-only, refusing to follow a symlink at the final
//     component.
//   - OpenAt opens a name relative to an already-open directory, with the same
//     no-follow guarantee.
//   - ErrSymlink reports the no-follow refusal, so callers can attach their own
//     context and fail closed.
//
// The security property is **no-follow**: a symlink (unix) or reparse point
// (windows — symlink, junction, mount point) at the opened component is
// rejected, never traversed. This blocks a hostile asset tree from redirecting
// a read outside its own directory (e.g. a symlink to /etc/passwd or a secret
// file). The rejection is atomic — the open itself carries the no-follow flag,
// so there is no check-then-open (TOCTOU) window on the final component.
//
// On unix this is O_NOFOLLOW (plus O_CLOEXEC/O_NONBLOCK/O_RDONLY), and OpenAt is
// a true fd-relative openat, so neither the leaf nor a parent directory can be
// swapped for a symlink mid-walk. On windows (the explicit follow-up, gated by
// the Windows CI job — same deferral posture as #623's Job Objects) the same
// seam is CreateFile with FILE_FLAG_OPEN_REPARSE_POINT, rejecting any handle
// whose FILE_ATTRIBUTE_REPARSE_POINT is set. Two windows differences are
// documented at their call sites and are tracked hardening, not silent gaps:
// OpenAt resolves against the directory's path rather than a directory handle
// (x/sys/windows exposes no fd-relative open without NtCreateFile), so it keeps
// the atomic per-leaf no-follow guarantee but not the fd-relative parent-swap
// guarantee; and the windows implementation is compiled but not yet exercised
// until the Windows CI job runs.
package safeopen
