//go:build unix && !linux && !darwin

package proc

import "time"

func startTime(int) (time.Time, bool) {
	return time.Time{}, false
}
