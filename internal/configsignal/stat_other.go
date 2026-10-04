//go:build !linux && !darwin

package configsignal

import (
	"os"
	"time"
)

// On other platforms the periodic content audit covers timestamp aliasing.
func changedTime(info os.FileInfo) int { return 0 }

func changedAt(info os.FileInfo) time.Time { return time.Time{} }
