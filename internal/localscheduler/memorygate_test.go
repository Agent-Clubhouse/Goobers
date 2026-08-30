package localscheduler

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/platform/memstat"
)

// gateAt builds a gate whose sampled reading is fixed, so the threshold
// arithmetic is tested without depending on the machine the test runs on.
func gateAt(t *testing.T, highWater float64, footprint memstat.Footprint) *CgroupMemoryGate {
	t.Helper()
	gate := NewCgroupMemoryGate(highWater)
	gate.read = func() memstat.Footprint { return footprint }
	return gate
}

func cgroupAt(current, limit uint64) memstat.Footprint {
	return memstat.Footprint{Cgroup: &memstat.Cgroup{
		Current: current,
		Limit:   limit,
		Anon:    current / 4,
		File:    current - current/4,
	}}
}

func TestCgroupMemoryGateRefusesOnlyAboveTheHighWater(t *testing.T) {
	const limit = 10 * 1024 * 1024 * 1024
	for name, tc := range map[string]struct {
		current uint64
		want    bool
	}{
		// The #3949 pod's ordinary steady state. Refusing here would trade an
		// occasional OOM for a permanently idle scheduler.
		"steady state at 76%": {current: 7_600_000_000, want: false},
		"just below at 89%":   {current: 9_556_000_000, want: false},
		"exactly at 90%":      {current: limit / 10 * 9, want: true},
		// The measured near-miss that motivated the gate.
		"near miss at 99.5%": {current: 10_690_641_920, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			pressured, detail := gateAt(t, 0.90, cgroupAt(tc.current, limit)).UnderPressure()
			if pressured != tc.want {
				t.Fatalf("UnderPressure() = %v, want %v at %d/%d", pressured, tc.want, tc.current, limit)
			}
			if pressured && !strings.Contains(detail, "of limit") {
				t.Fatalf("detail = %q, want the measurement that justified the refusal", detail)
			}
			if !pressured && detail != "" {
				t.Fatalf("detail = %q, want empty when admitting", detail)
			}
		})
	}
}

// Outside a container there is no limit to be near. The gate must not refuse
// anything, or every developer machine and most CI would stop scheduling.
func TestCgroupMemoryGateFailsOpenWithoutALimit(t *testing.T) {
	for name, footprint := range map[string]memstat.Footprint{
		"no cgroup":        {},
		"unlimited cgroup": {Cgroup: &memstat.Cgroup{Current: 1 << 40}},
	} {
		t.Run(name, func(t *testing.T) {
			if pressured, _ := gateAt(t, 0.90, footprint).UnderPressure(); pressured {
				t.Fatal("UnderPressure() = true, want fail-open without a limit")
			}
		})
	}
}

func TestCgroupMemoryGateClampsAnUnusableThreshold(t *testing.T) {
	for name, highWater := range map[string]float64{
		"zero":         0,
		"negative":     -1,
		"above unity":  1.5,
		"unparseable*": 0, // What daemonMemoryGate passes for a bad env value.
	} {
		t.Run(name, func(t *testing.T) {
			gate := NewCgroupMemoryGate(highWater)
			if gate.highWater != defaultMemoryHighWater {
				t.Fatalf("highWater = %v, want the %v default", gate.highWater, defaultMemoryHighWater)
			}
		})
	}
	// A usable threshold must survive untouched, or the clamp is just a
	// constant.
	if gate := NewCgroupMemoryGate(0.75); gate.highWater != 0.75 {
		t.Fatalf("highWater = %v, want 0.75 preserved", gate.highWater)
	}
}

// Admission runs per dispatchable entry per tick and holds Conditions' lock,
// so the gate must not read the cgroup once per entry.
func TestCgroupMemoryGateCachesWithinItsTTL(t *testing.T) {
	const limit = 10 * 1024 * 1024 * 1024
	reads := 0
	now := time.Unix(0, 0)
	gate := NewCgroupMemoryGate(0.90)
	gate.now = func() time.Time { return now }
	gate.read = func() memstat.Footprint {
		reads++
		return cgroupAt(1024, limit)
	}

	for range 10 {
		gate.UnderPressure()
	}
	if reads != 1 {
		t.Fatalf("reads = %d, want 1 within the TTL", reads)
	}

	now = now.Add(memoryGateSampleTTL)
	gate.UnderPressure()
	if reads != 2 {
		t.Fatalf("reads = %d, want a re-read once the TTL elapsed", reads)
	}
}

func TestNilCgroupMemoryGateAdmits(t *testing.T) {
	var gate *CgroupMemoryGate
	if pressured, _ := gate.UnderPressure(); pressured {
		t.Fatal("UnderPressure() = true on a nil gate, want fail-open")
	}
}

// stubMemoryGate is a MemoryGate with a fixed answer, for the admission tests.
type stubMemoryGate struct {
	pressured bool
	detail    string
	calls     int
}

func (g *stubMemoryGate) UnderPressure() (bool, string) {
	g.calls++
	return g.pressured, g.detail
}

func TestAdmitRefusesNewRunsUnderMemoryPressure(t *testing.T) {
	conditions := NewConditions()
	gate := &stubMemoryGate{pressured: true, detail: "9.5Gi at 95% of limit"}
	conditions.SetMemoryGate(gate)

	ok, reason := conditions.Admit("build", apiv1.ReadinessConditions{}, time.Now())
	if ok {
		t.Fatal("Admit() = true, want a refusal under memory pressure")
	}
	if !strings.HasPrefix(reason, ReasonMemoryPressure) {
		t.Fatalf("reason = %q, want the %q prefix", reason, ReasonMemoryPressure)
	}
	if !strings.Contains(reason, gate.detail) {
		t.Fatalf("reason = %q, want the measurement appended", reason)
	}
	// A refusal must not consume a budget slot or leak a reservation, or
	// backpressure would silently spend the workflow's hourly budget.
	if got := conditions.Active("build"); got != 0 {
		t.Fatalf("active count = %d, want 0 after a refusal", got)
	}
}

func TestAdmitProceedsWhenMemoryGateIsClear(t *testing.T) {
	conditions := NewConditions()
	gate := &stubMemoryGate{}
	conditions.SetMemoryGate(gate)

	ok, reason := conditions.Admit("build", apiv1.ReadinessConditions{}, time.Now())
	if !ok {
		t.Fatalf("Admit() = false (%s), want admission with a clear gate", reason)
	}
	if gate.calls == 0 {
		t.Fatal("the memory gate was never consulted")
	}
}

// An unwired gate must behave exactly as it did before #3949 added one.
func TestAdmitIsUnaffectedWithoutAMemoryGate(t *testing.T) {
	conditions := NewConditions()
	if ok, reason := conditions.Admit("build", apiv1.ReadinessConditions{}, time.Now()); !ok {
		t.Fatalf("Admit() = false (%s), want admission with no gate wired", reason)
	}
}

// Resuming is not a new start: it holds checkpoints and has already charged
// its memory, so refusing it strands work without lowering the ceiling.
func TestReserveContinuationIgnoresMemoryPressure(t *testing.T) {
	conditions := NewConditions()
	conditions.SetMemoryGate(&stubMemoryGate{pressured: true, detail: "9.9Gi at 99% of limit"})

	ok, reason := conditions.ReserveContinuation(WorkflowIdentity{Workflow: "build"}, apiv1.ReadinessConditions{})
	if !ok {
		t.Fatalf("ReserveContinuation() = false (%s), want an in-flight run to resume", reason)
	}
}

// The memory gate is the last check, so a run refused for a configured cap
// still reports that cap — the operator's own limit is the more actionable
// diagnostic, and the gate must not mask it.
func TestConfiguredCapsAreReportedBeforeMemoryPressure(t *testing.T) {
	conditions := NewConditions()
	conditions.SetMemoryGate(&stubMemoryGate{pressured: true})
	conditions.SetInstanceLimits(1, nil, nil)

	if ok, _ := conditions.Admit("first", apiv1.ReadinessConditions{}, time.Now()); ok {
		t.Fatal("the first Admit should have been refused by the memory gate")
	}
	conditions.SetMemoryGate(nil)
	if ok, reason := conditions.Admit("first", apiv1.ReadinessConditions{}, time.Now()); !ok {
		t.Fatalf("Admit() = false (%s), want admission once the gate is clear", reason)
	}

	conditions.SetMemoryGate(&stubMemoryGate{pressured: true})
	ok, reason := conditions.Admit("second", apiv1.ReadinessConditions{}, time.Now())
	if ok {
		t.Fatal("Admit() = true, want a refusal at the instance cap")
	}
	if reason != ReasonInstanceMaxParallel {
		t.Fatalf("reason = %q, want %q to win over memory pressure", reason, ReasonInstanceMaxParallel)
	}
}
