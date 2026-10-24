package localscheduler

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// releasableStarter holds each started run until the test finishes it.
type releasableStarter struct{ finish chan struct{} }

func (r releasableStarter) Start(ctx context.Context, _ StartRequest) (StartResult, error) {
	select {
	case <-r.finish:
	case <-ctx.Done():
	}
	return StartResult{}, nil
}

type claimFixture struct {
	t       *testing.T
	s       *Scheduler
	starter releasableStarter
	now     time.Time
}

// newClaimFixture is a scheduler whose workflows each admit maxConcurrent
// runs; started runs block until finished.
func newClaimFixture(t *testing.T, maxConcurrent int32, workflows ...string) *claimFixture {
	t.Helper()
	caps := map[string]int32{}
	for _, name := range workflows {
		caps[name] = maxConcurrent
	}
	return newClaimFixtureCaps(t, caps)
}

// newClaimFixtureCaps is newClaimFixture with a cap per workflow.
func newClaimFixtureCaps(t *testing.T, caps map[string]int32) *claimFixture {
	t.Helper()
	f := &claimFixture{t: t, starter: releasableStarter{finish: make(chan struct{})}, now: time.Unix(1_800_000_000, 0)}
	t.Cleanup(func() { close(f.starter.finish) })
	var entries []WorkflowEntry
	for name, maxConcurrent := range caps {
		entries = append(entries, WorkflowEntry{Workflow: name, Gaggle: "own", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub},
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: maxConcurrent, MaxRunsPerHour: 1000}, Starter: f.starter})
	}
	f.s, _ = newTestScheduler(t, entries)
	f.s.conditions.clock = func() time.Time { return f.now }
	return f
}

// park admits runID of workflow as a continuation and parks it on a child.
func (f *claimFixture) park(runID, workflow string) (*ChildParentSuspension, func()) {
	f.t.Helper()
	release, ok, reason := f.s.ReserveContinuation(runID, "own", workflow)
	if !ok {
		f.t.Fatalf("admit parent %s: %s", runID, reason)
	}
	wait, err := f.s.SuspendChildParent(f.t.Context(), runID)
	if err != nil {
		f.t.Fatal(err)
	}
	return wait, release
}

func (f *claimFixture) trigger(workflow string) error {
	f.t.Helper()
	_, err := f.s.Trigger(f.t.Context(), workflow, time.Now())
	return err
}

// finishOne lets one started run end and waits for its slot to free.
func (f *claimFixture) finishOne(workflow string) {
	f.t.Helper()
	identity := WorkflowIdentity{Gaggle: "own", Workflow: workflow}
	before := f.s.conditions.ActiveWorkflow(identity)
	f.starter.finish <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for f.s.conditions.ActiveWorkflow(identity) >= before {
		if time.Now().After(deadline) {
			f.t.Fatal("slot never freed")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func refusedFor(err error, reason string) bool {
	var refused *TriggerRejectedError
	return errors.As(err, &refused) && refused.Reason == reason
}

// #8174, inverted: once a parked parent's resume has been refused for
// capacity, the next slot that frees is its own, not a new trigger's. The
// trigger is refused as the capacity it is, and stays retryable.
func TestWaitingParentResumesAheadOfNewTriggers(t *testing.T) {
	f := newClaimFixture(t, 1, "parent")
	wait, release := f.park(strings.Repeat("a", 32), "parent")
	defer release()
	if err := f.trigger("parent"); err != nil { // the lent slot, by design
		t.Fatal(err)
	}
	if err := wait.Resume(t.Context()); !refusedFor(err, ReasonMaxParallel) {
		t.Fatalf("resume over a full pool = %v", err)
	}
	f.finishOne("parent")
	err := f.trigger("parent")
	var refused *TriggerRejectedError
	if !refusedFor(err, ReasonMaxParallel) || !errors.As(err, &refused) || !refused.Transient() {
		t.Fatalf("a new trigger while a parent waits = %v, want a transient capacity refusal", err)
	}
	if err := wait.Resume(t.Context()); err != nil {
		t.Fatalf("the waiting parent's resume = %v, want its slot", err)
	}
	if len(f.s.conditions.claims) != 0 {
		t.Fatalf("claims after resume = %v", f.s.conditions.claims)
	}
}

// A parked parent that has not asked to resume (its child still runs) holds
// nothing: its lent slot stays fully usable.
func TestParkedParentThatHasNotAskedDoesNotHoldCapacity(t *testing.T) {
	f := newClaimFixture(t, 1, "parent")
	_, release := f.park(strings.Repeat("a", 32), "parent")
	defer release()
	if err := f.trigger("parent"); err != nil {
		t.Fatal(err)
	}
	f.finishOne("parent")
	if err := f.trigger("parent"); err != nil {
		t.Fatalf("a trigger while a parent is parked but not waiting = %v, want admitted", err)
	}
}

// Waiting parents resume first come, first served.
func TestWaitingParentsResumeInTheOrderTheyStartedWaiting(t *testing.T) {
	f := newClaimFixture(t, 2, "parent")
	first, releaseFirst := f.park(strings.Repeat("a", 32), "parent")
	defer releaseFirst()
	second, releaseSecond := f.park(strings.Repeat("b", 32), "parent")
	defer releaseSecond()
	for range 2 {
		if err := f.trigger("parent"); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Resume(t.Context()); err == nil {
		t.Fatal("first parent resumed over a full pool")
	}
	f.now = f.now.Add(time.Second)
	if err := second.Resume(t.Context()); err == nil {
		t.Fatal("second parent resumed over a full pool")
	}
	f.finishOne("parent")
	if err := second.Resume(t.Context()); !refusedFor(err, ReasonMaxParallel) {
		t.Fatalf("the later waiter's resume with one slot free = %v, want it to yield to the earlier one", err)
	}
	if err := first.Resume(t.Context()); err != nil {
		t.Fatalf("the earlier waiter's resume = %v", err)
	}
	f.finishOne("parent")
	if err := second.Resume(t.Context()); err != nil {
		t.Fatalf("the later waiter's resume once its turn came = %v", err)
	}
}

// A claim whose parent stops asking expires: capacity is never held forever
// by a parent whose owner died.
func TestContinuationClaimExpiresWithoutRetries(t *testing.T) {
	f := newClaimFixture(t, 1, "parent")
	wait, release := f.park(strings.Repeat("a", 32), "parent")
	defer release()
	if err := f.trigger("parent"); err != nil {
		t.Fatal(err)
	}
	if err := wait.Resume(t.Context()); err == nil {
		t.Fatal("resumed over a full pool")
	}
	f.finishOne("parent")
	f.now = f.now.Add(ContinuationClaimTTL - time.Second)
	if err := f.trigger("parent"); !refusedFor(err, ReasonMaxParallel) {
		t.Fatalf("a trigger inside the claim's TTL = %v, want refused", err)
	}
	f.now = f.now.Add(time.Second)
	if err := f.trigger("parent"); err != nil {
		t.Fatalf("a trigger after the claim expired = %v, want admitted", err)
	}
}

// A refused retry renews the claim.
func TestContinuationClaimRenewsOnEachRefusedRetry(t *testing.T) {
	f := newClaimFixture(t, 1, "parent")
	wait, release := f.park(strings.Repeat("a", 32), "parent")
	defer release()
	if err := f.trigger("parent"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := wait.Resume(t.Context()); err == nil {
			t.Fatal("resumed over a full pool")
		}
		f.now = f.now.Add(ContinuationClaimTTL - time.Second)
	}
	f.finishOne("parent")
	if err := f.trigger("parent"); !refusedFor(err, ReasonMaxParallel) {
		t.Fatalf("a trigger while a retrying parent waits = %v, want refused", err)
	}
}

// A parent cancelled while waiting drops its claim: it can never resume.
func TestCancelledParentDropsItsClaim(t *testing.T) {
	f := newClaimFixture(t, 1, "parent")
	parent := strings.Repeat("a", 32)
	wait, release := f.park(parent, "parent")
	if err := f.trigger("parent"); err != nil {
		t.Fatal(err)
	}
	if err := wait.Resume(t.Context()); err == nil {
		t.Fatal("resumed over a full pool")
	}
	release() // the parent's own cleanup: cancelled while parked
	f.finishOne("parent")
	if err := f.trigger("parent"); err != nil {
		t.Fatalf("a trigger after the waiting parent was cancelled = %v, want admitted", err)
	}
	if err := wait.Resume(t.Context()); err == nil {
		t.Fatal("a cancelled parent resumed")
	}
}

// The instance cap binds the same way: a parent waiting on the shared pool
// takes the next instance slot ahead of another workflow's new trigger.
func TestWaitingParentHoldsTheInstancePool(t *testing.T) {
	f := newClaimFixture(t, 5, "parent", "other")
	f.s.conditions.SetInstanceLimits(1, nil, nil)
	wait, release := f.park(strings.Repeat("a", 32), "parent")
	defer release()
	if err := f.trigger("other"); err != nil {
		t.Fatal(err)
	}
	if err := wait.Resume(t.Context()); !refusedFor(err, ReasonInstanceMaxParallel) {
		t.Fatalf("resume over a full instance pool = %v", err)
	}
	f.finishOne("other")
	if err := f.trigger("other"); !refusedFor(err, ReasonInstanceMaxParallel) {
		t.Fatalf("another workflow's trigger while a parent waits on the pool = %v, want refused", err)
	}
	if err := wait.Resume(t.Context()); err != nil {
		t.Fatalf("the waiting parent's resume = %v", err)
	}
}

// Child admissions are new admissions too: another family's child cannot
// take the slot a waiting parent is owed.
func TestWaitingParentHoldsCapacityAgainstChildAdmissions(t *testing.T) {
	f := newClaimFixture(t, 1, "parent")
	wait, release := f.park(strings.Repeat("a", 32), "parent")
	defer release()
	if err := f.trigger("parent"); err != nil {
		t.Fatal(err)
	}
	if err := wait.Resume(t.Context()); err == nil {
		t.Fatal("resumed over a full pool")
	}
	f.finishOne("parent")
	other := strings.Repeat("c", 32)
	otherRelease, ok, reason := f.s.ReserveContinuation(other, "own", "parent")
	if ok {
		otherRelease()
		t.Fatal("an intervention continuation took a waiting parent's slot")
	}
	if reason != ReasonMaxParallel {
		t.Fatalf("intervention refusal = %q", reason)
	}
	_, request := childAdmissionFixture()
	request.ParentRunID, request.Parent = other, WorkflowIdentity{Gaggle: "own", Workflow: "parent"}
	request.Child.Gaggle = "own"
	if _, err := f.s.ReserveChild(t.Context(), request, f.now); err == nil {
		t.Fatal("another family's child took a waiting parent's slot")
	}
}

// With no claims, every admission decision is what it was before #8174:
// identical random operation sequences on two trackers, one of which has
// claims recorded and cleared (or expired) between operations, decide and
// count exactly alike.
func TestNoClaimsMeansUnchangedAdmissionDecisions(t *testing.T) {
	rng := rand.New(rand.NewSource(8174))
	identities := []WorkflowIdentity{{Gaggle: "own", Workflow: "a"}, {Gaggle: "own", Workflow: "b"}}
	readiness := apiv1.ReadinessConditions{MaxConcurrentRuns: 2, MaxRunsPerHour: 1000}
	for trial := range 50 {
		clock := time.Unix(1_800_000_000, 0)
		plain, touched := NewConditions(), NewConditions()
		for _, c := range []*Conditions{plain, touched} {
			c.SetInstanceLimits(3, nil, nil)
			c.clock = func() time.Time { return clock }
		}
		for step := range 200 {
			identity := identities[rng.Intn(len(identities))]
			// Claims come and go on one tracker only, never live at a decision.
			if rng.Intn(3) == 0 {
				touched.mu.Lock()
				touched.markContinuationWaitingLocked("run-"+identity.Workflow, identity, int(readiness.MaxConcurrentRuns), clock)
				touched.mu.Unlock()
				if rng.Intn(2) == 0 {
					touched.ClearContinuationClaim("run-" + identity.Workflow)
				} else {
					clock = clock.Add(ContinuationClaimTTL)
				}
			}
			clock = clock.Add(time.Second)
			switch rng.Intn(3) {
			case 0:
				a, ra := plain.AdmitWorkflow(identity, readiness, clock)
				b, rb := touched.AdmitWorkflow(identity, readiness, clock)
				if a != b || ra != rb {
					t.Fatalf("trial %d step %d: admit = %v %q vs %v %q", trial, step, a, ra, b, rb)
				}
			case 1:
				a, ra := plain.ReserveContinuation(identity, readiness)
				b, rb := touched.ReserveContinuation(identity, readiness)
				if a != b || ra != rb {
					t.Fatalf("trial %d step %d: continuation = %v %q vs %v %q", trial, step, a, ra, b, rb)
				}
			default:
				plain.ReleaseWorkflow(identity)
				touched.ReleaseWorkflow(identity)
			}
			for _, id := range identities {
				if plain.ActiveWorkflow(id) != touched.ActiveWorkflow(id) {
					t.Fatalf("trial %d step %d: active %v = %d vs %d", trial, step, id, plain.ActiveWorkflow(id), touched.ActiveWorkflow(id))
				}
			}
		}
	}
}

// A parent still waiting on its OWN workflow cap cannot use an instance slot,
// so its claim must not hold one: a parent of another workflow with room
// resumes, and another workflow's trigger is admitted (#8185 review P2).
func TestWorkflowCappedClaimDoesNotFreezeTheInstancePool(t *testing.T) {
	setup := func(t *testing.T) (*claimFixture, *ChildParentSuspension) {
		t.Helper()
		f := newClaimFixture(t, 1, "parent", "other", "third")
		f.s.conditions.SetInstanceLimits(2, nil, nil)
		p, releaseP := f.park(strings.Repeat("a", 32), "parent")
		t.Cleanup(releaseP)
		p2, releaseP2 := f.park(strings.Repeat("b", 32), "other")
		t.Cleanup(releaseP2)
		if err := f.trigger("parent"); err != nil { // X takes the parent's lent slot
			t.Fatal(err)
		}
		if err := p.Resume(t.Context()); !refusedFor(err, ReasonMaxParallel) {
			t.Fatalf("parent resume = %v, want max-parallel (its own workflow is full)", err)
		}
		f.now = f.now.Add(time.Second)
		return f, p2
	}
	t.Run("younger parent of another workflow", func(t *testing.T) {
		_, p2 := setup(t)
		if err := p2.Resume(t.Context()); err != nil {
			t.Fatalf("resume of a parent whose workflow and instance have room = %v", err)
		}
	})
	t.Run("another workflow's trigger", func(t *testing.T) {
		f, _ := setup(t)
		if err := f.trigger("third"); err != nil {
			t.Fatalf("a third workflow's trigger with an instance slot free = %v", err)
		}
	})
}

// Renewals keep a claim's place: two parents retrying interleaved resume in
// the order they first started waiting, however their retries alternate.
func TestInterleavedRetriesKeepFirstComeFirstServed(t *testing.T) {
	f := newClaimFixture(t, 2, "parent")
	first, releaseFirst := f.park(strings.Repeat("a", 32), "parent")
	defer releaseFirst()
	second, releaseSecond := f.park(strings.Repeat("b", 32), "parent")
	defer releaseSecond()
	for range 2 {
		if err := f.trigger("parent"); err != nil {
			t.Fatal(err)
		}
	}
	for _, wait := range []*ChildParentSuspension{first, second, second, first} {
		if err := wait.Resume(t.Context()); err == nil {
			t.Fatal("resumed over a full pool")
		}
		f.now = f.now.Add(time.Second)
	}
	f.finishOne("parent")
	if err := second.Resume(t.Context()); !refusedFor(err, ReasonMaxParallel) {
		t.Fatalf("the later waiter (renewed earlier) resumed first: %v", err)
	}
	if err := first.Resume(t.Context()); err != nil {
		t.Fatalf("the first waiter's resume = %v", err)
	}
}

// A watchdog force-releasing a waiting parent's admission drops its claim.
func TestReleaseRunDropsTheParentsClaim(t *testing.T) {
	f := newClaimFixture(t, 1, "parent")
	parent := strings.Repeat("a", 32)
	wait, release := f.park(parent, "parent")
	defer release()
	if err := f.trigger("parent"); err != nil {
		t.Fatal(err)
	}
	if err := wait.Resume(t.Context()); err == nil {
		t.Fatal("resumed over a full pool")
	}
	f.s.ReleaseRun(parent, "parent")
	if len(f.s.conditions.claims) != 0 {
		t.Fatalf("claims after ReleaseRun = %v", f.s.conditions.claims)
	}
	f.finishOne("parent")
	if err := f.trigger("parent"); err != nil {
		t.Fatalf("a trigger after the waiting parent was released = %v", err)
	}
}

// Reported worker availability nets out a waiting parent's claim on the pool.
func TestWorkerAvailabilityNetsOutWaitingClaims(t *testing.T) {
	f := newClaimFixture(t, 5, "parent", "other")
	f.s.conditions.SetInstanceLimits(2, nil, nil)
	wait, release := f.park(strings.Repeat("a", 32), "parent")
	defer release()
	for range 2 {
		if err := f.trigger("other"); err != nil {
			t.Fatal(err)
		}
	}
	if err := wait.Resume(t.Context()); !refusedFor(err, ReasonInstanceMaxParallel) {
		t.Fatalf("resume over a full pool = %v", err)
	}
	f.finishOne("other")
	if available, observable := f.s.conditions.WorkerAvailability(); !observable || available != 0 {
		t.Fatalf("availability with a free slot the waiting parent holds = %d (observable %v), want 0", available, observable)
	}
}

// Of two parents waiting on one workflow's single slot, only the older can
// use an instance slot; the younger one holds none until its turn.
func TestYoungerSameWorkflowWaiterHoldsNoInstanceSlot(t *testing.T) {
	f := newClaimFixtureCaps(t, map[string]int32{"parent": 1, "other": 5, "third": 5})
	f.s.conditions.SetInstanceLimits(2, nil, nil)
	older, releaseOlder := f.park(strings.Repeat("a", 32), "parent")
	defer releaseOlder()
	younger, releaseYounger := f.park(strings.Repeat("b", 32), "parent")
	defer releaseYounger()
	for range 2 {
		if err := f.trigger("other"); err != nil {
			t.Fatal(err)
		}
	}
	if err := older.Resume(t.Context()); err == nil {
		t.Fatal("older parent resumed over a full pool")
	}
	f.now = f.now.Add(time.Second)
	if err := younger.Resume(t.Context()); err == nil {
		t.Fatal("younger parent resumed over a full pool")
	}
	f.finishOne("other")
	f.finishOne("other")
	if err := f.trigger("third"); err != nil {
		t.Fatalf("a third workflow's trigger with one pool slot owed to the older parent and one free = %v", err)
	}
	if err := older.Resume(t.Context()); err != nil {
		t.Fatalf("the older parent's resume = %v", err)
	}
}
