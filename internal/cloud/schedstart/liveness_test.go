package schedstart

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
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const (
	liveWorkflow = "repair"
	liveGaggle   = "own"
)

type liveFixture struct {
	queue     *triggerqueue.Store
	log       *journal.InstanceLog
	scheduler *localscheduler.Scheduler
	service   startintent.Service
	started   chan string
	tracker   Tracker
}

func newLiveFixture(t *testing.T, log *journal.InstanceLog, readiness apiv1.ReadinessConditions, options ...localscheduler.Option) *liveFixture {
	t.Helper()
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "starts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	if log == nil {
		log, _, err = journal.OpenInstanceLog(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = log.Close() })
	}
	entry := localscheduler.WorkflowEntry{Workflow: liveWorkflow, Gaggle: liveGaggle, Starter: nilStarter{}, Readiness: readiness}
	f := &liveFixture{queue: queue, log: log, started: make(chan string, 32)}
	f.scheduler = localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, options...)
	f.service = startintent.Service{
		Queue: queue, Now: time.Now,
		Capture: func(context.Context, startintent.Request) (startintent.Target, func(), error) {
			return startintent.Target{Gaggle: liveGaggle, Workflow: liveWorkflow, ConfigGeneration: "g1", WorkflowDigest: "w1", GooberDigest: "d1"}, func() {}, nil
		},
		Build: func(context.Context, startintent.Target) (startintent.Prepared, error) {
			return startintent.Prepared{Entry: entry, Release: func() {}}, nil
		},
		Scheduler:    func() *localscheduler.Scheduler { return f.scheduler },
		RunDirectory: func(context.Context, string) (string, error) { return "", nil },
	}
	return f
}

func (f *liveFixture) config(terminal Terminal) Config {
	return Config{
		Engine: engineFunc(func(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
			f.started <- in.RunID
			return engine.StartResult{RunID: in.RunID}, nil
		}),
		Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
			return engine.RunInput{RunID: r.ID[len("trigger-"):]}, nil
		},
		Terminal:        terminal,
		TerminalBackoff: Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond},
	}
}

func (f *liveFixture) accept(t *testing.T, n int, prefix string) {
	t.Helper()
	for i := range n {
		if _, _, err := f.service.Accept(t.Context(), fmt.Sprintf("%s%d", prefix, i), "alice", startintent.Request{Workflow: liveWorkflow}); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *liveFixture) drain(t *testing.T, ctx context.Context, config Config) {
	t.Helper()
	d := Drain{Pager: f.queue, Service: f.service, Config: config, Tracker: &f.tracker}
	if _, err := d.Run(ctx, ""); err != nil {
		t.Fatal(err)
	}
}

func (f *liveFixture) finished(t *testing.T) []journal.Event {
	t.Helper()
	events, err := f.log.ReadAfterSeq(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []journal.Event
	for _, e := range events {
		if e.Type == journal.EventRunFinished {
			out = append(out, e)
		}
	}
	return out
}

func (f *liveFixture) expectNoStart(t *testing.T, why string) {
	t.Helper()
	select {
	case run := <-f.started:
		t.Fatalf("run %s started: %s", run, why)
	case <-time.After(150 * time.Millisecond):
	}
}

// A failing observation of a LIVE run must not free its slot or journal a
// false run.finished: Start retries until a real outcome.
func TestTransientTerminalErrorKeepsTheSlotAndJournalsNoFinish(t *testing.T) {
	f := newLiveFixture(t, nil, apiv1.ReadinessConditions{MaxConcurrentRuns: 1})
	var failures atomic.Int32
	closeRun := make(chan struct{})
	config := f.config(func(ctx context.Context, _ string) (Outcome, error) {
		if failures.Add(1) <= 4 {
			return Outcome{}, errors.New("temporal unavailable")
		}
		select {
		case <-closeRun:
			return Outcome{Phase: journal.PhaseCompleted}, nil
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	})
	var reports atomic.Int32
	config.Report = func(error) { reports.Add(1) }
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.accept(t, 2, "k")
	f.drain(t, epoch, config)
	<-f.started
	f.expectNoStart(t, "the slot of the first run was freed by a transient Terminal error")
	if got := f.finished(t); len(got) != 0 {
		t.Fatalf("false run.finished journaled for a live run: %+v", got)
	}
	f.drain(t, epoch, config)
	f.expectNoStart(t, "a retry freed the slot while the run was still live")
	if reports.Load() == 0 {
		t.Fatal("transient errors were not reported")
	}

	close(closeRun)
	deadline := time.Now().Add(5 * time.Second)
	for len(f.finished(t)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := f.finished(t)
	if len(got) != 1 || got[0].Status != string(journal.PhaseCompleted) {
		t.Fatalf("run.finished = %+v, want exactly one completed", got)
	}
	cancel()
	f.scheduler.Wait()
}

// Reconciled predecessor runs hold their slots only until they close: the
// watcher releases each through ReleaseReconciled.
func TestReconciledPredecessorSlotsReleaseWhenTheRunsClose(t *testing.T) {
	a := localscheduler.WorkflowIdentity{Gaggle: liveGaggle, Workflow: liveWorkflow}
	open := []OpenRun{{Identity: a, RunID: "pred-1"}, {Identity: a, RunID: "pred-2"}}
	f := newLiveFixture(t, nil, apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
		localscheduler.WithRunDirReconciler(NewReconciler(func(context.Context) ([]OpenRun, error) { return open, nil })))
	if err := f.scheduler.Reconcile("", time.Now()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	closed := map[string]chan struct{}{"pred-1": make(chan struct{}), "pred-2": make(chan struct{})}
	terminal := func(ctx context.Context, runID string) (Outcome, error) {
		mu.Lock()
		ch, ok := closed[runID]
		mu.Unlock()
		if !ok { // a run this epoch started: stays live
			<-ctx.Done()
			return Outcome{}, ctx.Err()
		}
		select {
		case <-ch:
			return Outcome{Phase: journal.PhaseCompleted}, nil
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	config := f.config(terminal)
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	config.WatchReconciled(epoch, open, f.scheduler.ReleaseReconciled, &f.tracker)

	f.accept(t, 3, "k")
	f.drain(t, epoch, config)
	f.expectNoStart(t, "predecessor runs fill MaxConcurrentRuns=2")

	close(closed["pred-1"])
	deadline := time.Now().Add(5 * time.Second)
	started := false
	for !started && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		f.drain(t, epoch, config)
		select {
		case <-f.started:
			started = true
		default:
		}
	}
	if !started {
		t.Fatal("a closed predecessor run never freed its slot")
	}
	f.expectNoStart(t, "only one slot should have been released")
	cancel()
	f.scheduler.Wait()
	waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := f.tracker.Wait(waitCtx); err != nil {
		t.Fatalf("watchers outlived the epoch: %v", err)
	}
}

// The rolling hourly budget survives a takeover: the successor rebuilds it from
// the instance log's run.started history, so the predecessor's start counts.
func TestSuccessorEnforcesTheHourlyBudgetFromTheInstanceLog(t *testing.T) {
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	readiness := apiv1.ReadinessConditions{MaxConcurrentRuns: 4, MaxRunsPerHour: 1}

	predecessor := newLiveFixture(t, log, readiness)
	closeRun := make(chan struct{})
	config := predecessor.config(func(ctx context.Context, _ string) (Outcome, error) {
		select {
		case <-closeRun:
			return Outcome{Phase: journal.PhaseCompleted}, nil
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	})
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	predecessor.accept(t, 1, "p")
	predecessor.drain(t, epoch, config)
	<-predecessor.started
	close(closeRun)
	deadline := time.Now().Add(5 * time.Second)
	for len(predecessor.finished(t)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	predecessor.scheduler.Wait()

	successor := newLiveFixture(t, log, readiness)
	if err := successor.scheduler.Reconcile("", time.Now()); err != nil {
		t.Fatal(err)
	}
	successor.accept(t, 1, "s")
	successor.drain(t, epoch, successor.config(func(ctx context.Context, _ string) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}))
	successor.expectNoStart(t, "the successor ignored the predecessor's hourly budget charge (MaxRunsPerHour=1)")
	cancel()
	successor.scheduler.Wait()
}
