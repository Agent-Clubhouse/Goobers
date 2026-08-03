//go:build windows

package proc

import (
	"time"

	"golang.org/x/sys/windows"
)

func startTime(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	// Unlike /proc on Linux or sysctl kern.proc.pid on Darwin — both of which
	// stop answering the moment a process is fully reaped — Windows keeps a
	// process object (and answers OpenProcess/GetProcessTimes) for as long as
	// ANY handle still references it, which can outlive the process actually
	// exiting. Require STILL_ACTIVE explicitly so an exited-but-not-yet-
	// destroyed process reads as "no answer," matching the other platforms
	// and the exited-process case pidReused actually needs to distinguish.
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil || code != stillActive {
		return time.Time{}, false
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, creation.Nanoseconds()), true
}
