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

type timer struct {
	C    <-chan time.Time
	stop func() bool
}

type wallTimerFactory func(time.Duration) (<-chan time.Time, func() bool)

func (t *timer) Stop() bool {
	if t == nil || t.stop == nil {
		return false
	}
	return t.stop()
}

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
	timer := newTimer(timeout)
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

func newTimer(timeout time.Duration) *timer {
	newWallTimer := func(timeout time.Duration) (<-chan time.Time, func() bool) {
		timer := time.NewTimer(timeout)
		return timer.C, timer.Stop
	}
	if timeout <= 0 {
		return newTimerWithSources(timeout, unbiasedUptime, nil, func() {}, newWallTimer)
	}
	interval := min(timeout, activeTimePollInterval)
	ticker := time.NewTicker(interval)
	return newTimerWithSources(timeout, unbiasedUptime, ticker.C, ticker.Stop, newWallTimer)
}

func newTimerWithSources(
	timeout time.Duration,
	uptime func() (time.Duration, error),
	polls <-chan time.Time,
	stopPolls func(),
	newWallTimer wallTimerFactory,
) *timer {
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

	start, err := uptime()
	if err != nil {
		stopPolls()
		fallback, stopFallback := newWallTimer(timeout)
		return &timer{C: fallback, stop: stopFallback}
	}
	if timeout <= 0 {
		stopPolls()
		once.Do(func() { fired <- time.Now() })
		return &timer{C: fired, stop: stopTimer}
	}

	go func() {
		defer stopPolls()
		last := start
		for {
			select {
			case now := <-polls:
				current, err := uptime()
				if err != nil {
					remaining := timeout - (last - start)
					if remaining <= 0 {
						once.Do(func() { fired <- now })
						return
					}
					fallback, stopFallback := newWallTimer(remaining)
					select {
					case fallbackNow := <-fallback:
						once.Do(func() { fired <- fallbackNow })
					case <-stop:
						stopFallback()
					}
					return
				}
				last = current
				if current-start >= timeout {
					once.Do(func() { fired <- now })
					return
				}
			case <-stop:
				return
			}
		}
	}()
	return &timer{C: fired, stop: stopTimer}
}

func unbiasedUptime() (time.Duration, error) {
	var ticks100ns uint64
	result, _, callErr := queryUnbiasedInterruptTime.Call(uintptr(unsafe.Pointer(&ticks100ns)))
	if result == 0 {
		return 0, callErr
	}
	return time.Duration(ticks100ns) * 100 * time.Nanosecond, nil
}
