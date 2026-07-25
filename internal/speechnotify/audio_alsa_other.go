//go:build !linux

package speechnotify

func alsaPlaybackAvailable() bool {
	return false
}
