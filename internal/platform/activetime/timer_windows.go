//go:build windows

package activetime

import (
	"context"
	"errors"
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

// Deadline reports the WALL-CLOCK instant the timeout would expire if the host
// never suspended: time.Now()+timeout at creation, capped by the parent's own
// deadline. It is not moved forward when the host suspends, so after a resume
// it can lie in the past while the context is still live, because Done() fires
// on active time and the suspended interval does not count. Callers that need
// to know whether the budget is spent must watch Done()/Err(), not compare
// Deadline() with time.Now(). Nothing in the executor does the latter today.
func (c deadlineContext) Deadline() (time.Time, bool) {
	if parentDeadline, ok := c.Context.Deadline(); ok && parentDeadline.Before(c.deadline) {
		return parentDeadline, true
	}
	return c.deadline, true
}

func (c deadlineContext) Err() error {
	err := c.Context.Err()
	if err != nil && errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
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

// suspendNoiseFloor absorbs the difference between the two clocks' update
// granularity (the unbiased interrupt time advances in timer-tick steps of
// about 15.6ms), so an unsuspended host never reports a phantom suspension.
const suspendNoiseFloor = time.Second

// Mark records one instant so a later SuspendedSince can report how much of
// the elapsed time the host spent suspended. See the package documentation.
// The zero Mark, or one taken while the unbiased clock was unavailable,
// reports no suspension, which preserves the plain monotonic comparison.
type Mark struct {
	wall   time.Time
	active time.Duration
	ok     bool
}

// NewMark records the current instant on both Go's monotonic clock (which on
// Windows includes suspension) and the unbiased interrupt time (which does
// not).
func NewMark() Mark {
	return newMarkWithSources(time.Now, unbiasedUptime)
}

func newMarkWithSources(now func() time.Time, uptime func() (time.Duration, error)) Mark {
	active, err := uptime()
	if err != nil {
		return Mark{}
	}
	return Mark{wall: now(), active: active, ok: true}
}

// SuspendedSince reports how long the host was suspended since m was taken:
// monotonic elapsed time minus unbiased elapsed time. It reports zero when
// either clock is unavailable or the gap is below suspendNoiseFloor.
func (m Mark) SuspendedSince() time.Duration {
	return m.suspendedSinceWithSources(time.Now, unbiasedUptime)
}

func (m Mark) suspendedSinceWithSources(now func() time.Time, uptime func() (time.Duration, error)) time.Duration {
	if !m.ok {
		return 0
	}
	active, err := uptime()
	if err != nil {
		return 0
	}
	suspended := now().Sub(m.wall) - (active - m.active)
	if suspended < suspendNoiseFloor {
		return 0
	}
	return suspended
}
