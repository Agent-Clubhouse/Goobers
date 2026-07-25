//go:build linux

package speechnotify

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

func alsaPlaybackAvailable() bool {
	paths, err := filepath.Glob("/dev/snd/pcmC*D*p")
	if err != nil {
		return false
	}
	for _, path := range paths {
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			_ = unix.Close(fd)
			return true
		}
	}
	return false
}
