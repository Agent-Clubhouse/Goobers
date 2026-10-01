package localscheduler

import (
	"strings"
	"testing"
	"time"
	"unsafe"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// afterHostSleep returns base advanced by wall of wall-clock time while its
// monotonic reading stays where it was — what time.Now returns on a macOS
// host that slept for wall, since Go's darwin monotonic clock
// (mach_absolute_time) does not advance during sleep. No public API can build
// such a value, so it edits the wall seconds field of the runtime layout
// directly; the preconditions below fail loudly if that layout ever changes.
func afterHostSleep(t *testing.T, base time.Time, wall time.Duration) time.Time {
	t.Helper()
	if !strings.Contains(base.String(), " m=") {
		t.Fatal("precondition: base must carry a monotonic reading")
	}
	type timeLayout struct {
		wall uint64
		ext  int64
		loc  *time.Location
	}
	if unsafe.Sizeof(time.Time{}) != unsafe.Sizeof(timeLayout{}) {
		t.Fatal("precondition: time.Time layout changed")
	}
	const nsecShift = 30 // time.Time stores wall seconds above 30 nanosecond bits when monotonic
	slept := base
	(*timeLayout)(unsafe.Pointer(&slept)).wall += uint64(wall/time.Second) << nsecShift
	if got := slept.Round(0).Sub(base.Round(0)); got != wall {
		t.Fatalf("precondition: wall-clock advance = %v, want %v", got, wall)
	}
	if got := slept.Sub(base); got != 0 {
		t.Fatalf("precondition: monotonic advance = %v, want 0 (host asleep)", got)
	}
	return slept
}

// TestHourlyBudgetWindowIsWallClockAcrossHostSleep is #6169: a start admitted
// before the host slept still counted toward maxRunsPerHour after it woke,
// hours of wall time later, because the window compared monotonic readings.
func TestHourlyBudgetWindowIsWallClockAcrossHostSleep(t *testing.T) {
	identity := WorkflowIdentity{Gaggle: "soak", Workflow: "pr-remediation"}
	readiness := apiv1.ReadinessConditions{MaxConcurrentRuns: 2, MaxRunsPerHour: 1}
	c := NewConditions()

	before := time.Now()
	if ok, reason := c.AdmitWorkflow(identity, readiness, before); !ok {
		t.Fatalf("first admit refused: %s", reason)
	}
	c.ReleaseWorkflow(identity)

	// Still inside the hour on both clocks: the budget holds.
	if ok, reason := c.AdmitWorkflow(identity, readiness, before.Add(30*time.Minute)); ok || reason != ReasonBudget {
		t.Fatalf("admit inside the hour = %v %q, want refused %q", ok, reason, ReasonBudget)
	}

	// Two hours of wall time later, but asleep the whole time.
	woke := afterHostSleep(t, before, 2*time.Hour)
	if ok, reason := c.AdmitWorkflow(identity, readiness, woke); !ok {
		t.Fatalf("admit two wall-clock hours later refused: %s (budget window used the monotonic clock)", reason)
	}
}

// TestSchedulerDefaultClockIsWallClock pins the scheduler's own deadlines
// (idle backoff, backlog/refill polls, auth-circuit retries, trigger-stall
// silence) to wall time for the same reason.
func TestSchedulerDefaultClockIsWallClock(t *testing.T) {
	sched, _ := newTestScheduler(t, nil)
	if now := sched.now(); strings.Contains(now.String(), " m=") {
		t.Fatalf("scheduler clock reading %v carries a monotonic component", now)
	}
}
