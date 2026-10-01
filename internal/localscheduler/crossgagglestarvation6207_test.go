package localscheduler

// Tests for #6207: two gaggles schedule a same-named workflow on offset
// minutes against a shared instance pool. The first gaggle's runs hold the
// pool every time the second gaggle's schedule fires, so an ordinary
// (unsized) schedule tick refused for instance max-parallel must be retained
// for the next freed slot, not dropped until its next cron minute — where it
// meets the same full pool and starves indefinitely.

import (
	"context"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// heldStarter blocks every Start until releaseAll, with a fresh hold per
// call, so each schedule period can model its own long-running run.
type heldStarter struct {
	mu     sync.Mutex
	starts int
	holds  []chan struct{}
}

func (h *heldStarter) Start(ctx context.Context, _ StartRequest) (StartResult, error) {
	hold := make(chan struct{})
	h.mu.Lock()
	h.starts++
	h.holds = append(h.holds, hold)
	h.mu.Unlock()
	select {
	case <-hold:
	case <-ctx.Done():
	}
	return StartResult{Phase: journal.PhaseCompleted}, nil
}

func (h *heldStarter) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.starts
}

func (h *heldStarter) releaseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, hold := range h.holds {
		close(hold)
	}
	h.holds = nil
}

type crossGaggleFixture struct {
	sched  *Scheduler
	dir    string
	area   *heldStarter
	strict *fakeStarter
}

// newCrossGaggleFixture is the ado-multi soak in miniature: area runs
// implementation and merge-review at :x5, strict runs merge-review at :x6,
// and the instance pool holds two runs.
func newCrossGaggleFixture(t *testing.T, start time.Time) crossGaggleFixture {
	t.Helper()
	area := &heldStarter{}
	t.Cleanup(area.releaseAll)
	strict := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	readiness := apiv1.ReadinessConditions{MaxConcurrentRuns: 1}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{Gaggle: "area", Workflow: "implementation", Schedules: []Schedule{mustParseSchedule(t, "5-59/10 * * * *")}, Readiness: readiness, Starter: area},
		{Gaggle: "area", Workflow: "merge-review", Schedules: []Schedule{mustParseSchedule(t, "5-59/10 * * * *")}, Readiness: readiness, Starter: area},
		{Gaggle: "strict", Workflow: "merge-review", Schedules: []Schedule{mustParseSchedule(t, "6-59/10 * * * *")}, Readiness: readiness, Starter: strict},
	})
	sched.conditions.SetInstanceLimits(2, nil, nil)
	for _, identity := range []WorkflowIdentity{
		{Gaggle: "area", Workflow: "implementation"},
		{Gaggle: "area", Workflow: "merge-review"},
		{Gaggle: "strict", Workflow: "merge-review"},
	} {
		setLastEval(sched, identity, start)
	}
	return crossGaggleFixture{sched: sched, dir: dir, area: area, strict: strict}
}

func drainWake(s *Scheduler) bool {
	select {
	case <-s.wake:
		return true
	default:
		return false
	}
}

func TestOffsetSameNamedWorkflowInOtherGaggleIsNotStarvedByInstancePool(t *testing.T) {
	start := time.Date(2026, time.October, 1, 15, 4, 0, 0, time.UTC)
	f := newCrossGaggleFixture(t, start)
	ctx := context.Background()

	const periods = 4
	for p := range periods {
		base := start.Add(time.Duration(p) * 10 * time.Minute)
		// :x5 — area fills both slots with long-running runs.
		f.sched.Tick(ctx, base.Add(time.Minute))
		waitForCount(t, f.area.count, 2*(p+1))
		// :x6 — strict's fire meets the full pool.
		f.sched.Tick(ctx, base.Add(2*time.Minute))
		if got := f.strict.count(); got != p {
			t.Fatalf("period %d: strict starts while pool full = %d, want %d", p, got, p)
		}
		// Area's runs finish shortly after :x6. Releasing a slot with a
		// retained fire outstanding must wake the Run loop.
		drainWake(f.sched)
		f.area.releaseAll()
		f.sched.Wait()
		if !drainWake(f.sched) {
			t.Fatalf("period %d: a freed slot with retained schedule demand did not wake the scheduler", p)
		}
		// The woken tick, before strict's next cron minute, takes the slot.
		f.sched.Tick(ctx, base.Add(2*time.Minute+30*time.Second))
		waitForCount(t, f.strict.count, p+1)
		f.sched.Wait()
	}
	if got := f.strict.count(); got != periods {
		t.Fatalf("strict merge-review starts = %d over %d periods, want one per period", got, periods)
	}
	if got := len(starvedEvents(t, f.dir)); got != 0 {
		t.Fatalf("workflow.starved events = %d, want 0 once refused fires are retained", got)
	}
}

func TestRetainedScheduleFireCoalescesAndClearsItsMarker(t *testing.T) {
	start := time.Date(2026, time.October, 1, 15, 4, 0, 0, time.UTC)
	f := newCrossGaggleFixture(t, start)
	ctx := context.Background()
	strict := WorkflowIdentity{Gaggle: "strict", Workflow: "merge-review"}

	f.sched.Tick(ctx, start.Add(time.Minute)) // 15:05 area fills the pool
	waitForCount(t, f.area.count, 2)
	f.sched.Tick(ctx, start.Add(2*time.Minute)) // 15:06 strict refused
	// Area's runs outlive a whole period: 15:16 is refused again and must
	// coalesce with the retained 15:06 fire rather than queue a second run.
	f.sched.Tick(ctx, start.Add(12*time.Minute))
	if got := f.strict.count(); got != 0 {
		t.Fatalf("strict starts while pool full = %d, want 0", got)
	}
	outstanding, err := readScheduleDemandState(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !outstanding[strict] {
		t.Fatalf("refused fire must be persisted as outstanding demand: %v", outstanding)
	}

	f.area.releaseAll()
	f.sched.Wait()
	f.sched.Tick(ctx, start.Add(12*time.Minute+30*time.Second))
	waitForCount(t, f.strict.count, 1)
	f.sched.Wait()
	f.sched.Tick(ctx, start.Add(13*time.Minute))
	f.sched.Wait()
	if got := f.strict.count(); got != 1 {
		t.Fatalf("strict starts = %d, want exactly 1 for coalesced refused fires", got)
	}
	outstanding, err = readScheduleDemandState(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if outstanding[strict] {
		t.Fatalf("admitted fire must clear its outstanding marker: %v", outstanding)
	}
}

// A fresh fire that coalesces with a retained one consumes the retained
// marker when admitted, so the next tick does not run it a second time.
func TestFreshFireAdmittedWithRetainedDemandDoesNotDoubleDispatch(t *testing.T) {
	start := time.Date(2026, time.October, 1, 15, 4, 0, 0, time.UTC)
	f := newCrossGaggleFixture(t, start)
	ctx := context.Background()

	f.sched.Tick(ctx, start.Add(time.Minute)) // 15:05 area fills the pool
	waitForCount(t, f.area.count, 2)
	f.sched.Tick(ctx, start.Add(2*time.Minute)) // 15:06 strict refused, retained
	f.area.releaseAll()
	f.sched.Wait()
	drainWake(f.sched)
	// No tick until strict's own next minute: the fresh 15:16 fire and the
	// retained 15:06 fire are one run. Area's 15:15 fire is marked evaluated
	// so it does not compete for the pool here.
	for _, workflow := range []string{"implementation", "merge-review"} {
		setLastEval(f.sched, WorkflowIdentity{Gaggle: "area", Workflow: workflow}, start.Add(11*time.Minute+30*time.Second))
	}
	f.sched.Tick(ctx, start.Add(12*time.Minute))
	waitForCount(t, f.strict.count, 1)
	f.sched.Wait()
	f.sched.Tick(ctx, start.Add(13*time.Minute))
	f.sched.Wait()
	if got := f.strict.count(); got != 1 {
		t.Fatalf("strict starts = %d, want 1", got)
	}
}

// Only the shared instance pool retains a fire. A workflow refused for its
// own maxConcurrentRuns keeps today's drop-the-tick behavior: it must not
// queue a run behind itself.
func TestWorkflowCapRefusalStillDropsUnsizedScheduleFire(t *testing.T) {
	start := time.Date(2026, time.October, 1, 15, 4, 0, 0, time.UTC)
	starter := &heldStarter{}
	t.Cleanup(starter.releaseAll)
	identity := WorkflowIdentity{Gaggle: "area", Workflow: "merge-review"}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Gaggle:    identity.Gaggle,
		Workflow:  identity.Workflow,
		Schedules: []Schedule{mustParseSchedule(t, "* * * * *")},
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}})
	sched.conditions.SetInstanceLimits(5, nil, nil)
	setLastEval(sched, identity, start)
	ctx := context.Background()

	sched.Tick(ctx, start.Add(time.Minute))
	waitForCount(t, starter.count, 1)
	sched.Tick(ctx, start.Add(2*time.Minute)) // refused: workflow cap
	outstanding, err := readScheduleDemandState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if outstanding[identity] {
		t.Fatalf("a workflow-cap refusal must not retain the fire: %v", outstanding)
	}
	starter.releaseAll()
	sched.Wait()
	sched.Tick(ctx, start.Add(2*time.Minute+30*time.Second))
	sched.Wait()
	if got := starter.count(); got != 1 {
		t.Fatalf("starts = %d, want 1: the refused fire was dropped", got)
	}
}
