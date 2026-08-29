//go:build unix && !linux && !darwin

package proc

import "time"

const startTimeSupported = false

func startTime(int) (time.Time, bool) {
	return time.Time{}, false
}
