package journal

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/platform/activetime"
)

// A zero lastActivity means "not observed through this handle", not "idle since
// the beginning of time". The watchdog must decline to judge rather than
// escalate, because now.Sub(zeroTime) is the maximum Duration and exceeds every
// configurable timeout — so no setting can prevent the escalation.
//
// MEASURED (#3774): a mode-3 agentic stage was escalated 11 minutes into a
// 60-minute budget, reporting "no journal progress for 2562047h47m16s (last
// activity 0001-01-01T00:00:00Z; timeout 45m0s)" while its trace showed five
// recorded events. Raising the timeout to 90m changed nothing, which is the
// tell: no finite threshold beats an unbounded baseline.
//
// lastActivity advances on Append through THIS handle and via ObserveActivity,
// which only the local runner path calls. A pod-dispatched stage writes its
// events through the journal PLANE, so the handle the watchdog holds is never
// advanced and every long mode-3 stage looks infinitely stale.
func TestIfLastActivityBeforeDeclinesToJudgeAnUnobservedRun(t *testing.T) {
	run, err := Create(t.TempDir(), testIdentity(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	// Model the handle the watchdog actually held on the cluster: one whose
	// lastActivity was never advanced. Create() appends run.started through this
	// handle and so sets it, which is exactly why the defect does not reproduce
	// for a locally-executed run — only for one whose events arrive by another
	// path entirely.
	run.mu.Lock()
	run.lastActivity = time.Time{}
	run.mu.Unlock()

	if run.IfLastActivityBefore(time.Now(), func(time.Time) {
		t.Fatal("claim ran for a run with no observed activity")
	}) {
		t.Fatal("an unobserved run was claimed as stale; that escalates healthy work and no configuration can stop it")
	}

	// Once activity IS observed, staleness is judged normally — the watchdog
	// must still do its job, which is what makes this fail-safe rather than
	// simply disabled.
	run.ObserveActivity()
	if run.IfLastActivityBefore(time.Now().Add(-time.Hour), func(time.Time) {}) {
		t.Fatal("a run active moments ago was judged stale against an hour-old cutoff")
	}
	var claimedAt time.Time
	if !run.IfLastActivityBefore(time.Now().Add(time.Hour), func(at time.Time) { claimedAt = at }) {
		t.Fatal("an observed run was not judged against a future cutoff; the watchdog must still fire")
	}
	if claimedAt.IsZero() {
		t.Fatal("claim received a zero timestamp for an observed run")
	}
}

// Host suspension since the last activity is not inactivity (#5875). On
// Windows Go's monotonic clock keeps counting through a sleep, so a run that
// went quiet five minutes before a two-hour suspend read as two hours and five
// minutes idle on the first sweep after resume, and a 45m stall timeout
// escalated healthy work. The suspended interval must be discounted, while
// the same run with no suspension, or one idle past the timeout in ACTIVE
// time, is still judged stale.
func TestIfLastActivityBeforeDiscountsHostSuspension(t *testing.T) {
	start := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	now := start
	run, err := Create(t.TempDir(), testIdentity(), nil, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	var suspended time.Duration
	previous := suspendedSince
	suspendedSince = func(activetime.Mark) time.Duration { return suspended }
	t.Cleanup(func() { suspendedSince = previous })

	run.ObserveActivity()
	const timeout = 45 * time.Minute
	now = start.Add(2*time.Hour + 5*time.Minute)

	suspended = 2 * time.Hour
	if run.IfLastActivityBefore(now.Add(-timeout), func(time.Time) {
		t.Fatal("claim ran for a run idle only 5m of active time")
	}) {
		t.Fatal("a run idle 5m before a 2h host suspend was judged stale against a 45m timeout")
	}

	suspended = 0
	var claimedAt time.Time
	if !run.IfLastActivityBefore(now.Add(-timeout), func(at time.Time) { claimedAt = at }) {
		t.Fatal("a run idle 2h5m with no suspension was not judged stale; the watchdog must still fire")
	}
	if !claimedAt.Equal(start) {
		t.Fatalf("claim received %s, want the recorded activity time %s", claimedAt, start)
	}

	// Idle past the timeout in active time even after discounting the suspend.
	suspended = time.Hour
	if !run.IfLastActivityBefore(now.Add(-timeout), func(time.Time) {}) {
		t.Fatal("a run idle 1h5m of active time was not judged stale against a 45m timeout")
	}
}
