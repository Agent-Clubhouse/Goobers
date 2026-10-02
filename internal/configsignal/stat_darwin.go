package configsignal

import (
	"os"
	"syscall"
	"time"
)

func changedTime(info os.FileInfo) syscall.Timespec {
	return info.Sys().(*syscall.Stat_t).Ctimespec
}

func changedAt(info os.FileInfo) time.Time {
	ts := info.Sys().(*syscall.Stat_t).Ctimespec
	return time.Unix(int64(ts.Sec), int64(ts.Nsec))
}
