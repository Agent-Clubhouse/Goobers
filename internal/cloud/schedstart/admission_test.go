package schedstart

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// The point of routing cloud starts through TriggerPreparedOrdinary: the run
// conditions bind them. With MaxConcurrentRuns=2, six accepted starts launch
// exactly two; the rest wait in custody (never start) until a run closes and its
// slot frees, because the Starter blocks until the run is terminal.
func TestAdmissionHoldsTheSlotUntilTheRunIsTerminal(t *testing.T) {
	const workflow, gaggle = "repair", "own"
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "starts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	var mu sync.Mutex
	engineStarts := map[string]int{}
	release := map[string]chan struct{}{}
	started := make(chan string, 16)
	eng := engineFunc(func(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
		mu.Lock()
		engineStarts[in.RunID]++
		release[in.RunID] = make(chan struct{})
		mu.Unlock()
		started <- in.RunID
		return engine.StartResult{RunID: in.RunID}, nil
	})
	config := Config{
		Engine: eng,
		Resolve: func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
			return engine.RunInput{RunID: r.ID[len("trigger-"):]}, nil
		},
		Terminal: func(ctx context.Context, runID string) (Outcome, error) {
			mu.Lock()
			ch := release[runID]
			mu.Unlock()
			select {
			case <-ch:
				return Outcome{Phase: journal.PhaseCompleted}, nil
			case <-ctx.Done():
				return Outcome{}, ctx.Err()
			}
		},
	}
	entry := localscheduler.WorkflowEntry{
		Workflow: workflow, Gaggle: gaggle, Starter: nilStarter{},
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
	}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log)
	var tracker Tracker
	service := startintent.Service{
		Queue: queue, Now: time.Now,
		Capture: func(context.Context, startintent.Request) (startintent.Target, func(), error) {
			return startintent.Target{Gaggle: gaggle, Workflow: workflow, ConfigGeneration: "g1", WorkflowDigest: "w1", GooberDigest: "d1"}, func() {}, nil
		},
		Build: func(context.Context, startintent.Target) (startintent.Prepared, error) {
			return startintent.Prepared{Entry: entry, Release: func() {}}, nil
		},
		Scheduler:    func() *localscheduler.Scheduler { return scheduler },
		RunDirectory: func(context.Context, string) (string, error) { return "", nil },
	}
	for i := range 6 {
		if _, _, err := service.Accept(t.Context(), fmt.Sprintf("k%d", i), "alice", startintent.Request{Workflow: workflow}); err != nil {
			t.Fatal(err)
		}
	}
	reported := make(chan error, 16)
	drain := Drain{Pager: queue, Service: service, Config: config, Tracker: &tracker, Parallel: 4, Report: func(err error) { reported <- err }}
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()

	if _, err := drain.Run(epoch, ""); err != nil {
		t.Fatal(err)
	}
	var running []string
	for range 2 {
		running = append(running, <-started)
	}
	select {
	case extra := <-started:
		t.Fatalf("a third run %s started past MaxConcurrentRuns=2", extra)
	case <-time.After(150 * time.Millisecond):
	}

	// Closing one run frees exactly one slot for the next drain.
	mu.Lock()
	close(release[running[0]])
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	var next string
	for next == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if _, err := drain.Run(epoch, ""); err != nil {
			t.Fatal(err)
		}
		select {
		case next = <-started:
		default:
		}
	}
	if next == "" {
		t.Fatal("freed slot was never reused: the Starter did not release on terminal")
	}
	mu.Lock()
	for run, n := range engineStarts {
		if n != 1 {
			t.Errorf("run %s reached the engine %d times", run, n)
		}
	}
	mu.Unlock()

	// Epoch end: blocked starters unwind, nothing is left running.
	cancel()
	scheduler.Wait()
	waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := tracker.Wait(waitCtx); err != nil {
		t.Fatalf("dispatch goroutines outlived the epoch: %v", err)
	}
}

type engineFunc func(context.Context, engine.RunInput) (engine.StartResult, error)

func (f engineFunc) Start(ctx context.Context, in engine.RunInput) (engine.StartResult, error) {
	return f(ctx, in)
}

// nilStarter is the entry's placeholder: Drain replaces it per record, and the
// scheduler must never tick-start through it when a SourceQueue is wired.
type nilStarter struct{}

func (nilStarter) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	return localscheduler.StartResult{}, fmt.Errorf("schedstart: placeholder starter must be replaced per record")
}
