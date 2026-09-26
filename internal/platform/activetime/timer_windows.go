//go:build windows

package activetime

import (
	"context"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const activeTimePollInterval = time.Second

var queryUnbiasedInterruptTime = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryUnbiasedInterruptTime")

type deadlineContext struct {
	context.Context
	deadline time.Time
}

func (c deadlineContext) Deadline() (time.Time, bool) {
	if parentDeadline, ok := c.Context.Deadline(); ok && parentDeadline.Before(c.deadline) {
		return parentDeadline, true
	}
	return c.deadline, true
}

// WithTimeout cancels after timeout has elapsed on Windows' unbiased interrupt
// clock, which excludes time while the guest is suspended.
func WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	base, cancelCause := context.WithCancelCause(parent)
	ctx := deadlineContext{Context: base, deadline: time.Now().Add(timeout)}
	timer := NewTimer(timeout)
	go func() {
		defer timer.Stop()
		select {
		case <-timer.C:
			cancelCause(context.DeadlineExceeded)
		case <-base.Done():
		}
	}()
	return ctx, func() {
		timer.Stop()
		cancelCause(context.Canceled)
	}
}

func newTimer(timeout time.Duration) *Timer {
	fired := make(chan time.Time, 1)
	stop := make(chan struct{})
	var once sync.Once
	stopTimer := func() bool {
		stopped := false
		once.Do(func() {
			close(stop)
			stopped = true
		})
		return stopped
	}

	start, err := unbiasedUptime()
	if err != nil {
		timer := time.NewTimer(timeout)
		return &Timer{C: timer.C, stop: timer.Stop}
	}
	if timeout <= 0 {
		once.Do(func() { fired <- time.Now() })
		return &Timer{C: fired, stop: stopTimer}
	}

	interval := min(timeout, activeTimePollInterval)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				uptime, err := unbiasedUptime()
				if err != nil || uptime-start >= timeout {
					once.Do(func() { fired <- now })
					return
				}
			case <-stop:
				return
			}
		}
	}()
	return &Timer{C: fired, stop: stopTimer}
}

func unbiasedUptime() (time.Duration, error) {
	var ticks100ns uint64
	result, _, callErr := queryUnbiasedInterruptTime.Call(uintptr(unsafe.Pointer(&ticks100ns)))
	if result == 0 {
		return 0, callErr
	}
	return time.Duration(ticks100ns) * 100 * time.Nanosecond, nil
}
