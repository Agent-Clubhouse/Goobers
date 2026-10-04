package configsignal

import (
	"os"
	"syscall"
	"time"
)

func changedTime(info os.FileInfo) syscall.Timespec {
	return info.Sys().(*syscall.Stat_t).Ctim
}

func changedAt(info os.FileInfo) time.Time {
	ts := info.Sys().(*syscall.Stat_t).Ctim
	return time.Unix(ts.Unix())
}
