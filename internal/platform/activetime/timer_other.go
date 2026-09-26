//go:build !windows

package activetime

import "time"

func newTimer(timeout time.Duration) *Timer {
	timer := time.NewTimer(timeout)
	return &Timer{C: timer.C, stop: timer.Stop}
}
