//go:build cloudintegration

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudclaims "github.com/goobers/goobers/internal/cloud/claims"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/schedentries"
	"github.com/goobers/goobers/internal/cloud/schedstart"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type pinnedGenerations struct{}

func (pinnedGenerations) Acquire(context.Context, string) (childworkflow.Generation, error) {
	return childworkflow.Generation{Release: func() error { return nil }}, nil
}

// controllerPool opens a pool whose sessions act as goobers_controller, the role
// production runs as, so a write path missing from the grant inventory fails here.
func controllerPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := admin.Config().Copy()
	config.MaxConns = lease.MaxFencedConnections
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// world is the shared durable state two controller incarnations contend over, and
// the engine they both start runs on (a fake that confirms starts the way the
// start fence does, so run pins are real).
type world struct {
	t        *testing.T
	admin    *pgxpool.Pool
	pool     *pgxpool.Pool
	now      atomic.Int64 // unix nanos, the shared scheduler clock
	mu       sync.Mutex
	starts   map[string]int // run -> engine starts (any controller)
	live     int            // runs started and not yet released
	maxLive  int            // peak of live
	released map[string]chan struct{}
	closed   map[string]bool
}

func newWorld(t *testing.T) *world {
	t.Helper()
	admin := pgtest.Pool(t, "l1e")
	w := &world{t: t, admin: admin, pool: controllerPool(t, admin),
		starts: map[string]int{}, released: map[string]chan struct{}{}, closed: map[string]bool{}}
	w.now.Store(time.Now().UTC().Truncate(time.Minute).UnixNano())
	return w
}

func (w *world) clock() time.Time { return time.Unix(0, w.now.Load()).UTC() }

func (w *world) releaseRun(runID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed[runID] = true
	ch, ok := w.released[runID]
	if !ok {
		ch = make(chan struct{})
		w.released[runID] = ch
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
	if w.live > 0 {
		w.live--
	}
}

func (w *world) startCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.starts)
}

func (w *world) duplicates() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for run, n := range w.starts {
		if n != 1 {
			out = append(out, fmt.Sprintf("%s x%d", run, n))
		}
	}
	return out
}

// controller is one incarnation: lease, fenced stores and an admission.
type controller struct {
	w         *world
	holder    string
	manager   *lease.Manager
	handle    *lease.Handle
	store     *acceptance.Store
	backend   *journalpg.Backend
	epoch     context.Context
	cancel    context.CancelFunc
	admission *schedulerAdmission
}

func (w *world) controller(holder string, handle func(*lease.Manager) *lease.Handle, entry localscheduler.WorkflowEntry, withSources bool) *controller {
	return w.skewedController(holder, 0, handle, entry, withSources)
}

// skewedController is a controller whose node clock runs skew ahead of (or, when
// negative, behind) the shared test clock: separate pods have separate clocks.
func (w *world) skewedController(holder string, skew time.Duration, handle func(*lease.Manager) *lease.Handle, entry localscheduler.WorkflowEntry, withSources bool) *controller {
	w.t.Helper()
	clock := func() time.Time { return w.clock().Add(skew) }
	manager := pgtest.Manager(w.t, w.pool, holder, 0)
	h := handle(manager)
	store, err := acceptance.New(w.pool, pgtest.InstanceID)
	if err != nil {
		w.t.Fatal(err)
	}
	store.BindFence(manager, h)
	backend, err := journalpg.New(journalpg.Config{Pool: w.pool, InstanceID: pgtest.InstanceID, Lease: manager, LiveOwner: func() *lease.Handle { return h }, InstanceClock: clock})
	if err != nil {
		w.t.Fatal(err)
	}
	epochCtx, cancel := context.WithCancel(w.t.Context())
	c := &controller{w: w, holder: holder, manager: manager, handle: h, store: store, backend: backend, epoch: epochCtx, cancel: cancel}
	w.t.Cleanup(cancel)

	eng := engineStarter(func(ctx context.Context, in engine.RunInput) (engine.StartResult, error) {
		w.mu.Lock()
		w.starts[in.RunID]++
		first := w.starts[in.RunID] == 1
		if first {
			w.live++
			w.maxLive = max(w.maxLive, w.live)
			if _, ok := w.released[in.RunID]; !ok {
				w.released[in.RunID] = make(chan struct{})
			}
		}
		w.mu.Unlock()
		pins, err := acceptance.RunPinsFor(in)
		if err != nil {
			return engine.StartResult{}, err
		}
		if err := store.ConfirmStart(ctx, in.AcceptanceID, in.RunID, pins); err != nil {
			return engine.StartResult{}, err
		}
		return engine.StartResult{RunID: in.RunID}, nil
	})
	comp := schedulerComposition{
		Applied: func() (cloudconfig.Candidate, bool) { return cloudconfig.Candidate{Digest: "gen-1"}, true },
		OpenLog: func(ctx context.Context, report func(error)) (journal.InstanceStore, error) {
			return backend.OpenSchedulerLog(ctx, report)
		},
		State: func(deps control.AdmissionDeps) (localscheduler.SchedulerState, error) {
			return cloudclaims.New(w.pool, pgtest.InstanceID, manager, deps.Handle)
		},
		OpenRuns: func(ctx context.Context, deps control.AdmissionDeps) ([]schedstart.OpenRun, error) {
			pins, err := deps.Store.UnmarkedRunPins(ctx)
			if err != nil {
				return nil, err
			}
			uncertain, err := uncertainStarts(ctx, deps.Store)
			if err != nil {
				return nil, err
			}
			return collectOpenRuns(ctx, openRunInputs{Pins: pins, Uncertain: uncertain, Closed: func(_ context.Context, runID string) (bool, error) {
				w.mu.Lock()
				defer w.mu.Unlock()
				return w.closed[runID], nil
			}})
		},
		Terminal: func(ctx context.Context, runID string) (schedstart.Outcome, error) {
			w.mu.Lock()
			ch, ok := w.released[runID]
			if !ok {
				ch = make(chan struct{})
				w.released[runID] = ch
			}
			w.mu.Unlock()
			select {
			case <-ch:
				return schedstart.Outcome{Phase: journal.PhaseCompleted}, nil
			case <-ctx.Done():
				return schedstart.Outcome{}, ctx.Err()
			}
		},
		Entries: func(c cloudconfig.Candidate, _ generationCounters) ([]localscheduler.WorkflowEntry, error) {
			e := entry
			e.ConfigGeneration = c.Digest
			e.Starter = placeholder{}
			return []localscheduler.WorkflowEntry{e}, nil
		},
		Quota:   localscheduler.NewProviderQuotaState(),
		Now:     clock,
		Options: []localscheduler.Option{localscheduler.WithClock(clock, func(time.Duration) <-chan time.Time { return nil })},
	}
	if withSources {
		comp.Sources = func(deps control.AdmissionDeps, build func(context.Context, startintent.Target) (startintent.Prepared, error)) (localscheduler.SourceQueue, error) {
			return schedentries.NewSources(schedentries.Deps{Queue: deps.Store, Generations: pinnedGenerations{}, Build: build})
		}
	}
	deps := control.AdmissionDeps{
		Handle: h, Store: store, Starter: eng,
		Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
			envelope, err := startintent.Parse(r.Payload)
			if err != nil {
				return engine.RunInput{}, err
			}
			t := envelope.Target
			return engine.RunInput{
				AcceptanceID: r.ID, RunID: strings.TrimPrefix(r.ID, "trigger-"), Gaggle: t.Gaggle, WorkflowName: t.Workflow, Version: 1,
				WorkflowDigest: t.WorkflowDigest, GooberDigest: t.GooberDigest, ConfigGeneration: t.ConfigGeneration, SelfExecutionDenied: true,
			}, nil
		},
	}
	admission, err := comp.admission(epochCtx, deps)
	if err != nil {
		w.t.Fatal(err)
	}
	c.admission = admission.(*schedulerAdmission)
	return c
}

func testEntry(maxConcurrent int32) localscheduler.WorkflowEntry {
	return localscheduler.WorkflowEntry{
		Workflow: testWorkflow, Gaggle: testGaggle, ConfigGeneration: "gen-1",
		WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goober",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: maxConcurrent},
	}
}

func (c *controller) accept(n int, prefix string) {
	c.w.t.Helper()
	payload, err := startintent.Envelope{
		Kind: startintent.Kind, Request: startintent.Request{Workflow: testWorkflow},
		Target: startintent.Target{Gaggle: testGaggle, Workflow: testWorkflow, ConfigGeneration: "gen-1", WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goober"},
	}.Marshal()
	if err != nil {
		c.w.t.Fatal(err)
	}
	for i := range n {
		if _, _, err := c.store.Accept(c.w.t.Context(), fmt.Sprintf("%s%d", prefix, i), "alice", payload, c.w.clock()); err != nil {
			c.w.t.Fatal(err)
		}
	}
}

// tickConcurrently drives rounds of concurrent L1 ticks, as an overlapping host
// loop would, and waits for them.
func (c *controller) tickConcurrently(goroutines, rounds int) {
	c.w.t.Helper()
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range rounds {
				if err := c.admission.Tick(c.w.t.Context()); err != nil && c.epoch.Err() == nil {
					c.w.t.Errorf("tick: %v", err)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
	wg.Wait()
}

// tickDeposed ticks a deposed leader. Its fenced writes are refused, so a tick
// fails with the stale-epoch error (the host would end that epoch) and admits
// nothing.
func (c *controller) tickDeposed(rounds int) {
	c.w.t.Helper()
	for range rounds {
		if err := c.admission.Tick(c.w.t.Context()); err != nil && !errors.Is(err, lease.ErrStale) {
			c.w.t.Errorf("deposed tick failed with %v, want the stale-epoch refusal or success without admitting", err)
		}
	}
}

func (c *controller) stop() {
	c.w.t.Helper()
	c.cancel()
	stopAdmission(c.w.t, c.admission)
}

func (w *world) settle(want int, why string) {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if w.startCount() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.t.Fatalf("%s: engine starts = %d, want %d", why, w.startCount(), want)
}

// TestTwoControllersNeverOverAdmitAcrossATakeover is the P3 admission evidence:
// real PG, real leases, the real scheduler. The first leader admits up to
// MaxConcurrentRuns under concurrent ticks; after a takeover (expired lease,
// another holder) the successor rebuilds admission from the durable run pins, so
// the predecessor's open runs keep their slots, nothing is started twice, and a
// closed run frees exactly one slot.
func TestTwoControllersNeverOverAdmitAcrossATakeover(t *testing.T) {
	w := newWorld(t)
	first := w.controller("replica-1", func(m *lease.Manager) *lease.Handle { return pgtest.Acquire(t, m) }, testEntry(3), false)
	first.accept(8, "k")
	if err := first.admission.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	first.tickConcurrently(4, 6)
	w.settle(3, "first leader under concurrent ticks")
	if w.maxLive > 3 {
		t.Fatalf("peak live runs = %d, want at most MaxConcurrentRuns=3", w.maxLive)
	}

	// Takeover: the first lease expires on the database clock, replica-2 acquires.
	zombie := first
	second := w.controller("replica-2", func(m *lease.Manager) *lease.Handle { return pgtest.Takeover(t, w.admin, m) }, testEntry(3), false)
	if err := second.admission.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The deposed leader keeps ticking: its fenced writes are refused, so it
	// cannot admit anything.
	zombie.tickDeposed(3)
	second.tickConcurrently(4, 6)
	w.settle(3, "successor must count the predecessor's 3 open runs")

	// One predecessor run closes: exactly one slot frees for the next accepted row.
	var predecessor string
	w.mu.Lock()
	for run := range w.starts {
		predecessor = run
		break
	}
	w.mu.Unlock()
	w.releaseRun(predecessor)
	deadline := time.Now().Add(10 * time.Second)
	for w.startCount() < 4 && time.Now().Before(deadline) {
		second.tickConcurrently(2, 2)
	}
	w.settle(4, "one freed slot admits exactly one more run")
	second.tickConcurrently(4, 6)
	w.settle(4, "no further run without another freed slot")

	if dup := w.duplicates(); len(dup) != 0 {
		t.Fatalf("runs reached the engine more than once: %v", dup)
	}
	second.stop()
	first.stop()
}

// TestCronAtHandoverFiresOnceAndStartsOnce is CHAOS-10 through the real control
// composition: a due cron occurrence is accepted by the first leader and started
// once; after a takeover, the successor replaying the same tick accepts nothing
// new and starts nothing new, and the deposed leader cannot accept the next one.
func TestCronAtHandoverFiresOnceAndStartsOnce(t *testing.T) {
	w := newWorld(t)
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry := testEntry(4)
	entry.Schedules = []localscheduler.Schedule{schedule}
	first := w.controller("replica-1", func(m *lease.Manager) *lease.Handle { return pgtest.Acquire(t, m) }, entry, true)
	if err := first.admission.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	base := w.clock()
	w.now.Store(base.Add(time.Minute).UnixNano())
	first.tickConcurrently(1, 3)
	w.settle(1, "the first leader's due occurrence")
	accepted := func() int {
		var n int
		if err := w.admin.QueryRow(t.Context(), `SELECT count(*) FROM control.acceptance`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if accepted() != 1 {
		t.Fatalf("accepted occurrences = %d, want 1", accepted())
	}

	// Handover at the same instant: the successor replays the tick.
	second := w.controller("replica-2", func(m *lease.Manager) *lease.Handle { return pgtest.Takeover(t, w.admin, m) }, entry, true)
	if err := second.admission.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	second.tickConcurrently(3, 4)
	if accepted() != 1 || w.startCount() != 1 {
		t.Fatalf("handover replay: accepted %d, started %d; want 1 and 1", accepted(), w.startCount())
	}

	// The next occurrence: the deposed leader is refused; the new leader accepts
	// it exactly once and starts it exactly once.
	w.now.Store(base.Add(2 * time.Minute).UnixNano())
	first.tickDeposed(3)
	if accepted() != 1 {
		t.Fatalf("a deposed leader accepted an occurrence: %d", accepted())
	}
	second.tickConcurrently(3, 4)
	w.settle(2, "the new leader's occurrence")
	if accepted() != 2 {
		t.Fatalf("accepted occurrences = %d, want 2", accepted())
	}
	if dup := w.duplicates(); len(dup) != 0 {
		t.Fatalf("runs reached the engine more than once: %v", dup)
	}
	second.stop()
	first.stop()
}

// TestSuccessorWithAClockBehindDoesNotStallOrDoubleFire: the controllers are
// different pods, so the successor's clock can trail the predecessor's by
// seconds. The predecessor (6s ahead) stores an evaluation time that the
// successor's reconstruction (its own earlier now) would regress; the successor
// must still prepare, must not fire an occurrence twice, and must not miss the
// next one once its clock reaches it.
func TestSuccessorWithAClockBehindDoesNotStallOrDoubleFire(t *testing.T) {
	w := newWorld(t)
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry := testEntry(4)
	entry.Schedules = []localscheduler.Schedule{schedule}
	const skew = 6 * time.Second
	first := w.skewedController("replica-1", skew, func(m *lease.Manager) *lease.Handle { return pgtest.Acquire(t, m) }, entry, true)
	if err := first.admission.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	base := w.clock()
	first.tickConcurrently(1, 2) // nothing due; its Prepare stored base+6s
	if w.startCount() != 0 {
		t.Fatalf("an occurrence fired before it was due: %d", w.startCount())
	}

	// Immediate takeover onto a node whose clock is 6s behind the shared clock: it
	// reconstructs base-6s, earlier than the stored base+6s.
	second := w.skewedController("replica-2", -skew, func(m *lease.Manager) *lease.Handle { return pgtest.Takeover(t, w.admin, m) }, entry, true)
	if err := second.admission.Prepare(t.Context()); err != nil {
		t.Fatalf("a successor with a slower clock refused to prepare: %v", err)
	}
	second.tickConcurrently(2, 2)
	if w.startCount() != 0 {
		t.Fatalf("an occurrence fired early on the slow clock: %d", w.startCount())
	}

	// The schedule cursor is in the ahead leader's clock domain (base+6s), so on
	// the slow node (6s behind) the next occurrence is due one RELATIVE skew late:
	// 12s after the ideal minute. Not before, and not later than that.
	w.now.Store(base.Add(time.Minute).UnixNano())
	second.tickConcurrently(2, 2)
	if w.startCount() != 0 {
		t.Fatalf("the occurrence fired before it was due on the slow clock: %d", w.startCount())
	}
	w.now.Store(base.Add(time.Minute + 2*skew + time.Second).UnixNano())
	second.tickConcurrently(3, 3)
	w.settle(1, "the successor's first due occurrence (late by at most the relative skew)")
	// Re-evaluating the same span (the slow clock re-covers it) never fires it twice.
	second.tickConcurrently(3, 3)
	w.settle(1, "no duplicate fire after re-evaluation")
	var accepted int
	if err := w.admin.QueryRow(t.Context(), `SELECT count(*) FROM control.acceptance`).Scan(&accepted); err != nil || accepted != 1 {
		t.Fatalf("accepted occurrences = %d (%v), want 1", accepted, err)
	}
	if dup := w.duplicates(); len(dup) != 0 {
		t.Fatalf("runs reached the engine more than once: %v", dup)
	}
	second.stop()
	first.stop()
}
