//go:build !linux && !darwin

package configsignal

import "os"

// On other platforms the periodic content audit covers timestamp aliasing.
func changedTime(info os.FileInfo) int { return 0 }
