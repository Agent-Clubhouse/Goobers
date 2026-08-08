//go:build darwin

package proc

import (
	"time"

	"golang.org/x/sys/unix"
)

const startTimeSupported = true

func startTime(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	// SysctlKinfoProc returns EIO for an unmatched pid (the kernel writes
	// fewer bytes than sizeof(kinfo_proc) and the wrapper checks for exactly
	// that), so a returned error already covers "no such process" — no
	// separate zero-value check is needed.
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return time.Time{}, false
	}
	sec, nsec := info.Proc.P_starttime.Unix()
	return time.Unix(sec, nsec), true
}
