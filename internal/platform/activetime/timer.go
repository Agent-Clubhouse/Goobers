package activetime

import "time"

// Timer fires after the requested amount of active host time. On platforms
// that expose suspend-excluding uptime, time spent suspended is not charged.
type Timer struct {
	C    <-chan time.Time
	stop func() bool
}

// NewTimer starts an active-time timer.
func NewTimer(timeout time.Duration) *Timer {
	return newTimer(timeout)
}

// Stop prevents the timer from firing when it is still active.
func (t *Timer) Stop() bool {
	if t == nil || t.stop == nil {
		return false
	}
	return t.stop()
}
