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
package activetime
