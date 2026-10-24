package schedstart

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type fakeEngine struct {
	mu      sync.Mutex
	started []string
	result  engine.StartResult
	err     error
}

func (e *fakeEngine) Start(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started = append(e.started, in.RunID)
	if e.err != nil {
		return engine.StartResult{}, e.err
	}
	result := e.result
	result.RunID = in.RunID
	return result, nil
}

func record(runID string) triggerqueue.Record {
	return triggerqueue.Record{ID: "trigger-" + runID, State: triggerqueue.Accepted}
}

func config(e *fakeEngine, terminal Terminal) Config {
	return Config{
		Engine: e, Terminal: terminal,
		Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
			return engine.RunInput{RunID: r.ID[len("trigger-"):]}, nil
		},
	}
}

// The slot is held for the run's whole life: Start must not return until the
// terminal wait does, and must report that outcome.
func TestStartBlocksUntilTerminalAndReportsOutcome(t *testing.T) {
	release := make(chan struct{})
	e := &fakeEngine{}
	starter := config(e, func(_ context.Context, runID string) (Outcome, error) {
		<-release
		return Outcome{Phase: journal.PhaseFailed, FailureCode: "boom"}, nil
	}).StarterFor(record("run-1"), nil)
	done := make(chan localscheduler.StartResult, 1)
	go func() {
		result, err := starter.Start(t.Context(), localscheduler.StartRequest{RunID: "run-1"})
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	select {
	case <-done:
		t.Fatal("Start returned before the run was terminal; the scheduler slot would be released early")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	result := <-done
	if result.Phase != journal.PhaseFailed || result.FailureCode != "boom" {
		t.Fatalf("result = %+v", result)
	}
}

// An abandoned wait (epoch ended) is an error, never a fabricated terminal.
func TestStartAbandonedWaitIsAnErrorNotATerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	starter := config(&fakeEngine{}, func(ctx context.Context, _ string) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}).StarterFor(record("run-2"), nil)
	cancel()
	if result, err := starter.Start(ctx, localscheduler.StartRequest{RunID: "run-2"}); !errors.Is(err, context.Canceled) || result.Phase != "" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestStartRefusesAForeignRunAndANotMineExecution(t *testing.T) {
	waited := false
	terminal := func(context.Context, string) (Outcome, error) { waited = true; return Outcome{}, nil }
	e := &fakeEngine{result: engine.StartResult{NotMine: true}}
	starter := config(e, terminal).StarterFor(record("run-3"), nil)
	if _, err := starter.Start(t.Context(), localscheduler.StartRequest{RunID: "other"}); err == nil || len(e.started) != 0 {
		t.Fatalf("admitted run for a different acceptance: %v started=%v", err, e.started)
	}
	if _, err := starter.Start(t.Context(), localscheduler.StartRequest{RunID: "run-3"}); !errors.Is(err, ErrNotMine) || waited {
		t.Fatalf("NotMine err = %v waited = %v; must not hold a slot for another owner's run", err, waited)
	}
}

// A predecessor's own earlier start is adopted: the slot is held until it closes.
func TestStartAdoptsAnAlreadyRunningOwnStart(t *testing.T) {
	e := &fakeEngine{result: engine.StartResult{AlreadyRunning: true, Adopt: true}}
	starter := config(e, func(context.Context, string) (Outcome, error) {
		return Outcome{Phase: journal.PhaseCompleted}, nil
	}).StarterFor(record("run-4"), nil)
	result, err := starter.Start(t.Context(), localscheduler.StartRequest{RunID: "run-4"})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestTrackerWaitsForRegisteredDispatches(t *testing.T) {
	var tracker Tracker
	done := tracker.register()
	finished := make(chan error, 1)
	go func() { finished <- tracker.Wait(t.Context()) }()
	select {
	case <-finished:
		t.Fatal("Wait returned with a dispatch live")
	case <-time.After(30 * time.Millisecond):
	}
	done()
	done() // idempotent
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	release := tracker.register()
	cancel()
	if err := tracker.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait with a live dispatch and a dead context = %v", err)
	}
	release()
}

type fakePager struct{ rows []triggerqueue.Record }

func (p fakePager) RetainedPage(_ context.Context, after string, limit int) ([]triggerqueue.Record, error) {
	var out []triggerqueue.Record
	for _, r := range p.rows {
		if r.ID > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

// Many accepted rows, bounded parallelism, every row dispatched exactly once,
// no dispatch goroutine outlives Run, and one failing dispatch does not starve
// the rest.
func TestDrainDispatchesEachAcceptedRowOnceWithBoundedParallelism(t *testing.T) {
	const total = 60
	var rows []triggerqueue.Record
	for i := range total {
		r := record(fmt.Sprintf("run-%03d", i))
		if i%10 == 9 {
			r.State = triggerqueue.Dispatching // retained but not accepted: skipped
		}
		rows = append(rows, r)
	}
	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	seen := map[string]int{}
	d := Drain{Pager: fakePager{rows: rows}, Parallel: 4}
	var reported atomic.Int32
	d.Report = func(error) { reported.Add(1) }
	d.dispatch = func(_, _ context.Context, r triggerqueue.Record) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		seen[r.ID]++
		mu.Unlock()
		if r.ID == "trigger-run-005" {
			return errors.New("refused")
		}
		return nil
	}
	after, err := d.Run(t.Context(), "")
	if err != nil || after != "" {
		t.Fatalf("after = %q, err = %v", after, err)
	}
	if inFlight.Load() != 0 {
		t.Fatal("a dispatch goroutine outlived Run")
	}
	if peak.Load() > 4 {
		t.Fatalf("parallelism %d exceeded the bound", peak.Load())
	}
	want := total - total/10
	if len(seen) != want {
		t.Fatalf("dispatched %d rows, want %d", len(seen), want)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s dispatched %d times", id, n)
		}
	}
	if reported.Load() != 1 {
		t.Fatalf("reported %d, want exactly the one refused dispatch", reported.Load())
	}
}

func TestDrainStopsAndWaitsWhenTheEpochEnds(t *testing.T) {
	var rows []triggerqueue.Record
	for i := range 20 {
		rows = append(rows, record(fmt.Sprintf("run-%03d", i)))
	}
	ctx, cancel := context.WithCancel(t.Context())
	var live atomic.Int32
	d := Drain{Pager: fakePager{rows: rows}, Parallel: 2}
	d.dispatch = func(admission, _ context.Context, _ triggerqueue.Record) error {
		live.Add(1)
		defer live.Add(-1)
		cancel()
		<-admission.Done()
		return admission.Err()
	}
	if _, err := d.Run(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if live.Load() != 0 {
		t.Fatal("dispatch goroutines outlived the epoch")
	}
}

func TestReconcilerRebuildsAdmissionFromOpenRuns(t *testing.T) {
	a := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "a"}
	b := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "b"}
	r := NewReconciler(func(context.Context) ([]OpenRun, error) {
		return []OpenRun{
			{Identity: a, RunID: "1"}, {Identity: a, RunID: "2"}, {Identity: a, RunID: "2"},
			{Identity: b, RunID: "3", Suspended: true},
		}, nil
	})
	inv, err := r.FromRoots(t.Context(), []string{"ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Active[a] != 2 || inv.Active[b] != 0 || len(inv.Runs) != 3 || !inv.Runs["3"].Suspended {
		t.Fatalf("inventory = %+v", inv)
	}
	failing := NewReconciler(func(context.Context) ([]OpenRun, error) { return nil, errors.New("temporal down") })
	if _, err := failing.FromRoots(t.Context(), nil); err == nil {
		t.Fatal("enumeration failure must not become an empty inventory")
	}
}
