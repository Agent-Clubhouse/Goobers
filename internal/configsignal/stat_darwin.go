package configsignal

import (
	"os"
	"syscall"
)

func changedTime(info os.FileInfo) syscall.Timespec {
	return info.Sys().(*syscall.Stat_t).Ctimespec
}
