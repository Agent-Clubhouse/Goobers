package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/schedentries"
	"github.com/goobers/goobers/internal/cloud/schedstart"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const (
	testGaggle   = "own"
	testWorkflow = "repair"
)

var testIdentity = localscheduler.WorkflowIdentity{Gaggle: testGaggle, Workflow: testWorkflow}

type memoryState struct{}

func (memoryState) ReadScheduleDemand(string) (map[localscheduler.WorkflowIdentity]bool, error) {
	return map[localscheduler.WorkflowIdentity]bool{}, nil
}
func (memoryState) ReadTriggerEvaluations(string) (map[localscheduler.WorkflowIdentity]time.Time, error) {
	return map[localscheduler.WorkflowIdentity]time.Time{}, nil
}
func (memoryState) WriteScheduleDemand(string, map[localscheduler.WorkflowIdentity]bool) error {
	return nil
}
func (memoryState) WriteTriggerEvaluations(string, map[localscheduler.WorkflowIdentity]time.Time) error {
	return nil
}

type fakeCounters struct {
	runCtx  chan context.Context
	stopped atomic.Int32
}

func (c *fakeCounters) For(localscheduler.WorkflowIdentity) schedentries.Counters {
	return schedentries.Counters{}
}
func (c *fakeCounters) OpenPRs() localscheduler.OpenPRCounter { return nil }
func (c *fakeCounters) Run(ctx context.Context) error {
	c.runCtx <- ctx
	<-ctx.Done()
	c.stopped.Add(1)
	return nil
}

// admissionFixture wires a schedulerComposition over fakes and a real
// localscheduler, with a SQLite acceptance queue.
type admissionFixture struct {
	t        *testing.T
	comp     schedulerComposition
	queue    *triggerqueue.Store
	log      *journal.InstanceLog
	started  chan string
	release  sync.Map // runID -> chan struct{}
	applied  atomic.Pointer[cloudconfig.Candidate]
	open     atomic.Pointer[[]schedstart.OpenRun]
	counters []*fakeCounters
	quotas   []*localscheduler.ProviderQuotaState
	gates    *gateStatus
	entry    localscheduler.WorkflowEntry
	reports  []error
	mu       sync.Mutex
}

func newAdmissionFixture(t *testing.T, readiness apiv1.ReadinessConditions, withCounters bool) *admissionFixture {
	t.Helper()
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "starts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &admissionFixture{t: t, queue: queue, log: log, started: make(chan string, 32), gates: newGateStatus()}
	f.entry = localscheduler.WorkflowEntry{Workflow: testWorkflow, Gaggle: testGaggle, Readiness: readiness, ConfigGeneration: "gen-1"}
	f.setApplied("gen-1")
	f.open.Store(&[]schedstart.OpenRun{})
	f.comp = schedulerComposition{
		Applied: func() (cloudconfig.Candidate, bool) { c := f.applied.Load(); return *c, true },
		OpenLog: func(context.Context, func(error)) (journal.InstanceStore, error) { return f.log, nil },
		State:   func(control.AdmissionDeps) (localscheduler.SchedulerState, error) { return memoryState{}, nil },
		OpenRuns: func(context.Context, control.AdmissionDeps) ([]schedstart.OpenRun, error) {
			return *f.open.Load(), nil
		},
		Terminal: func(ctx context.Context, runID string) (schedstart.Outcome, error) {
			ch, _ := f.release.LoadOrStore(runID, make(chan struct{}))
			select {
			case <-ch.(chan struct{}):
				return schedstart.Outcome{Phase: journal.PhaseCompleted}, nil
			case <-ctx.Done():
				return schedstart.Outcome{}, ctx.Err()
			}
		},
		Entries: func(c cloudconfig.Candidate, _ generationCounters) ([]localscheduler.WorkflowEntry, error) {
			entry := f.entry
			entry.ConfigGeneration = c.Digest
			entry.Starter = placeholder{}
			return []localscheduler.WorkflowEntry{entry}, nil
		},
		Quota: localscheduler.NewProviderQuotaState(),
		Queue: func(control.AdmissionDeps) acceptanceQueue { return queue },
		Gates: f.gates,
		Report: func(err error) {
			f.mu.Lock()
			f.reports = append(f.reports, err)
			f.mu.Unlock()
		},
	}
	if withCounters {
		f.comp.Counters = func(_ cloudconfig.Candidate, quota *localscheduler.ProviderQuotaState) (generationCounters, error) {
			c := &fakeCounters{runCtx: make(chan context.Context, 1)}
			f.counters = append(f.counters, c)
			f.quotas = append(f.quotas, quota)
			return c, nil
		}
	}
	return f
}

type placeholder struct{}

func (placeholder) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	return localscheduler.StartResult{}, errors.New("placeholder starter must be replaced per record")
}

func (f *admissionFixture) setApplied(digest string) {
	f.applied.Store(&cloudconfig.Candidate{Digest: digest})
}

func (f *admissionFixture) deps() control.AdmissionDeps {
	return control.AdmissionDeps{
		Handle: &lease.Handle{InstanceID: "i", Epoch: 3},
		Starter: engineStarter(func(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
			f.started <- in.RunID
			return engine.StartResult{RunID: in.RunID}, nil
		}),
		Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
			return engine.RunInput{RunID: r.ID[len("trigger-"):]}, nil
		},
	}
}

type engineStarter func(context.Context, engine.RunInput) (engine.StartResult, error)

func (s engineStarter) Start(ctx context.Context, in engine.RunInput) (engine.StartResult, error) {
	return s(ctx, in)
}

func (f *admissionFixture) build(epoch context.Context) *schedulerAdmission {
	f.t.Helper()
	f.comp.Options = append(f.comp.Options, localscheduler.WithClock(time.Now, time.After))
	f.comp.Entries = wrapEntries(f.comp.Entries)
	admission, err := f.comp.admission(epoch, f.deps())
	if err != nil {
		f.t.Fatal(err)
	}
	// Capture is applied through the same Service the drain uses: give it a
	// Capture so Accept works.
	return admission.(*schedulerAdmission)
}

func wrapEntries(fn func(cloudconfig.Candidate, generationCounters) ([]localscheduler.WorkflowEntry, error)) func(cloudconfig.Candidate, generationCounters) ([]localscheduler.WorkflowEntry, error) {
	return fn
}

func (f *admissionFixture) accept(n int, prefix string) {
	f.t.Helper()
	service := startintent.Service{Queue: f.queue, Now: time.Now,
		Capture: func(context.Context, startintent.Request) (startintent.Target, func(), error) {
			return startintent.Target{Gaggle: testGaggle, Workflow: testWorkflow, ConfigGeneration: "gen-1", WorkflowDigest: "w1", GooberDigest: "d1"}, func() {}, nil
		}}
	for i := range n {
		if _, _, err := service.Accept(f.t.Context(), fmt.Sprintf("%s%d", prefix, i), "alice", startintent.Request{Workflow: testWorkflow}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *admissionFixture) expectNoStart(why string) {
	f.t.Helper()
	select {
	case run := <-f.started:
		f.t.Fatalf("run %s started: %s", run, why)
	case <-time.After(150 * time.Millisecond):
	}
}

func (f *admissionFixture) warnings() int {
	events, err := f.log.ReadAfterSeq(0)
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == journal.EventRunnerAnnotation && e.Name == "scheduler.gates-not-enforced" {
			n++
		}
	}
	return n
}

func stopAdmission(t *testing.T, a *schedulerAdmission) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := a.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// Stop must be safe when Prepare never ran, and when it is called twice.
func TestAdmissionStopIsSafeWithoutPrepare(t *testing.T) {
	f := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, true)
	a := f.build(t.Context())
	stopAdmission(t, a)
	stopAdmission(t, a)
	if err := a.Tick(t.Context()); err == nil {
		t.Fatal("Tick before Prepare must fail, not run a nil scheduler")
	}
}

// A predecessor's open run, and one whose start was still uncertain
// ('dispatching') at Prepare, both count against MaxConcurrentRuns.
func TestPrepareCountsOpenAndUncertainPredecessorRuns(t *testing.T) {
	f := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 2}, false)
	f.open.Store(&[]schedstart.OpenRun{
		{Identity: testIdentity, RunID: "open-run"},
		{Identity: testIdentity, RunID: "dispatching-run"}, // an unresolved dispatching row
	})
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := f.build(epoch)
	if err := a.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.accept(2, "k")
	if err := a.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.expectNoStart("predecessor runs fill MaxConcurrentRuns=2")
	// The open run closes: exactly one slot frees.
	ch, _ := f.release.LoadOrStore("open-run", make(chan struct{}))
	close(ch.(chan struct{}))
	deadline := time.Now().Add(5 * time.Second)
	started := false
	for !started && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if err := a.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-f.started:
			started = true
		default:
		}
	}
	if !started {
		t.Fatal("a closed predecessor run never freed its slot")
	}
	f.expectNoStart("only one slot was released")
	cancel()
	stopAdmission(t, a)
}

// Every goroutine derives from the epoch ctx: cancelling it ends the counters
// and Stop returns with nothing leaked, even though no Tick ctx was involved.
func TestAdmissionGoroutinesEndWithTheEpochContext(t *testing.T) {
	f := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, true)
	epoch, cancel := context.WithCancel(t.Context())
	a := f.build(epoch)
	if err := a.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	runCtx := <-f.counters[0].runCtx
	cancel() // the epoch ends
	select {
	case <-runCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("counters' context is not a child of the epoch context")
	}
	stopAdmission(t, a)
	if f.counters[0].stopped.Load() != 1 {
		t.Fatal("counters were not stopped")
	}
}

// A generation change reloads the scheduler (new entries) and swaps counters:
// the old Run is cancelled and its replacement starts, over the SAME quota.
func TestGenerationChangeReloadsEntriesAndSwapsCounters(t *testing.T) {
	f := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, true)
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := f.build(epoch)
	if err := a.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-f.counters[0].runCtx
	f.setApplied("gen-2")
	if err := a.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.counters) != 2 || f.counters[0].stopped.Load() != 1 {
		t.Fatalf("counters built = %d, first stopped = %d; want 2 and 1", len(f.counters), f.counters[0].stopped.Load())
	}
	<-f.counters[1].runCtx
	if f.quotas[0] != f.quotas[1] || f.quotas[0] != f.comp.Quota {
		t.Fatal("generation counters were built over different quota states")
	}
	if a.generation != "gen-2" || a.entries[testIdentity].ConfigGeneration != "gen-2" {
		t.Fatalf("entries not reloaded: generation %q entry %q", a.generation, a.entries[testIdentity].ConfigGeneration)
	}
	cancel()
	stopAdmission(t, a)
}

// ONE quota state binds the scheduler's gate and feeds the counters: exhausting
// the shared state stops admission.
func TestSharedQuotaStateGatesAdmission(t *testing.T) {
	f := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 4}, true)
	f.entry.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "o", Name: "r"}
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := f.build(epoch)
	if err := a.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.quotas[0] != f.comp.Quota {
		t.Fatal("counters were not given the process quota state")
	}
	f.comp.Quota.RecordExhausted(time.Now().Add(time.Hour))
	f.accept(1, "k")
	if err := a.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.expectNoStart("the shared quota state is exhausted")
	cancel()
	stopAdmission(t, a)
}

// With no counters composed, every provider-backed gate is operator-visible as a
// readiness check, and exactly one warning is journaled per epoch.
func TestDisabledGatesAreVisibleAndJournaledOncePerEpoch(t *testing.T) {
	f := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, false)
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := f.build(epoch)
	if err := a.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.setApplied("gen-2")
	if err := a.Tick(t.Context()); err != nil { // a generation change must not warn again
		t.Fatal(err)
	}
	checks := f.gates.Checks()
	for _, gate := range []string{"max-open-prs", "backlog", "schedule-demand", "refill"} {
		if enforced, present := checks["scheduler-gate:"+gate]; !present || enforced {
			t.Fatalf("gate %s checks = %v; want present and false", gate, checks)
		}
	}
	if n := f.warnings(); n != 1 {
		t.Fatalf("journaled warnings = %d, want exactly 1 per epoch", n)
	}
	cancel()
	stopAdmission(t, a)
}

// Real counters clear the disabled gates.
func TestComposedCountersClearTheGateWarnings(t *testing.T) {
	f := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, true)
	f.gates.Set("max-open-prs", "stale")
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := f.build(epoch)
	if err := a.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if checks := f.gates.Checks(); len(checks) != 0 {
		t.Fatalf("checks = %v, want none once counters are composed", checks)
	}
	if f.warnings() != 0 {
		t.Fatal("a warning was journaled although counters are composed")
	}
	cancel()
	stopAdmission(t, a)
}

var _ = schedstart.ErrNotMine
