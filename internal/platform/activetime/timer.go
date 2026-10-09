// Package activetime provides timers that exclude host suspension where the
// operating system exposes an active-runtime clock.
//
// It also provides Mark, for comparisons that must use Go's monotonic clock,
// such as the journal's in-memory stall check
// (journal.Run.IfLastActivityBefore). Go's monotonic clock already excludes
// suspension on Linux and macOS, but on Windows it is the interrupt time,
// which keeps counting while the host sleeps. There, a stage that was idle for
// five minutes before a two-hour suspend looks idle for two hours and five
// minutes after resume (#5875). Shifting the recorded instant forward by
// Mark.SuspendedSince removes the suspended interval, and leaves the
// comparison unchanged on every other platform.
//
// Mark is only useful against Go's monotonic clock. Timestamps persisted in a
// journal carry no monotonic reading, so comparing them across a suspend
// counts the suspended interval on every platform. WallMark reports that
// interval on every platform, so such comparisons can subtract it (#5891).
package activetime

import "time"

// suspendNoiseFloor absorbs the difference between two clocks' update
// granularity (Windows' unbiased interrupt time advances in timer-tick steps
// of about 15.6ms, and NTP slews the wall clock by a bounded rate), so an
// unsuspended host never reports a phantom suspension.
const suspendNoiseFloor = time.Second

// suspendedGap is the part of a wall-clock interval the active clock did not
// see, or zero below the noise floor.
func suspendedGap(wallElapsed, activeElapsed time.Duration) time.Duration {
	suspended := wallElapsed - activeElapsed
	if suspended < suspendNoiseFloor {
		return 0
	}
	return suspended
}
