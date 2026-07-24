package workerhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
)

type fakeWorker struct {
	queue    string
	opts     worker.Options
	startErr error

	mu      sync.Mutex
	started bool
	stopped bool
}

func (w *fakeWorker) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.startErr != nil {
		return w.startErr
	}
	w.started = true
	return nil
}

func (w *fakeWorker) Stop() {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
}

// isStarted reads started under the worker's own lock — Start runs on the
// h.Run goroutine, so a polling test must not read the flag bare.
func (w *fakeWorker) isStarted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.started
}

type fakeFleet struct {
	mu       sync.Mutex
	workers  []*fakeWorker
	startErr map[string]error
}

func (f *fakeFleet) newWorker(_ client.Client, queue string, opts worker.Options) managedWorker {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWorker{queue: queue, opts: opts, startErr: f.startErr[queue]}
	f.workers = append(f.workers, w)
	return w
}

func newTestHost(t *testing.T, cfg Config, fleet *fakeFleet) *Host {
	t.Helper()
	h, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.dial = func(string, string) (client.Client, error) { return nil, nil }
	h.newWorker = fleet.newWorker
	return h
}

func TestNewRequiresTaskQueues(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted a config with no task queues")
	}
	if _, err := New(Config{TaskQueues: []string{"a", ""}}); err == nil {
		t.Fatal("New accepted an empty task queue name")
	}
}

// TestRunServesEveryQueueAndDrains covers the core worker mechanics (#632):
// one worker per named queue, all under the versioned identity and the
// configured drain window, stopped on context cancellation, clean exit when
// nothing was in flight.
func TestRunServesEveryQueueAndDrains(t *testing.T) {
	fleet := &fakeFleet{}
	h := newTestHost(t, Config{
		HostPort:     "temporal.example:7233",
		Namespace:    "default",
		TaskQueues:   []string{"goobers-engine", "goobers-engine-web"},
		DrainTimeout: 7 * time.Second,
		BuildVersion: "v9.9.9-test",
	}, fleet)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	// Cancellation is the only shutdown trigger; give Run a moment to start.
	waitFor(t, func() bool {
		fleet.mu.Lock()
		defer fleet.mu.Unlock()
		// fleet.mu only guards the workers slice; each started flag has its
		// own lock.
		return len(fleet.workers) == 2 && fleet.workers[0].isStarted() && fleet.workers[1].isStarted()
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fleet.workers) != 2 {
		t.Fatalf("workers = %d, want one per queue", len(fleet.workers))
	}
	queues := []string{fleet.workers[0].queue, fleet.workers[1].queue}
	if queues[0] != "goobers-engine" || queues[1] != "goobers-engine-web" {
		t.Errorf("queues = %v, want configured order", queues)
	}
	for _, w := range fleet.workers {
		if !w.stopped {
			t.Errorf("worker for %q was not stopped on drain", w.queue)
		}
		if !strings.Contains(w.opts.Identity, "goobers-worker/v9.9.9-test@") {
			t.Errorf("identity %q does not carry the build version", w.opts.Identity)
		}
		if !strings.Contains(w.opts.Identity, fmt.Sprintf("#%d", os.Getpid())) {
			t.Errorf("identity %q does not carry the pid", w.opts.Identity)
		}
		if w.opts.WorkerStopTimeout != 7*time.Second {
			t.Errorf("WorkerStopTimeout = %v, want the drain timeout", w.opts.WorkerStopTimeout)
		}
		if len(w.opts.Interceptors) != 1 {
			t.Errorf("interceptors = %d, want the in-flight tracker", len(w.opts.Interceptors))
		}
	}
}

func TestRunReportsAbandonedWorkDistinctly(t *testing.T) {
	fleet := &fakeFleet{}
	h := newTestHost(t, Config{TaskQueues: []string{"goobers-engine"}}, fleet)

	// Simulate an activity still executing when the drain window expires.
	h.tracker.n.Add(1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.Run(ctx)
	if !errors.Is(err, ErrAbandonedWork) {
		t.Fatalf("Run = %v, want ErrAbandonedWork", err)
	}
}

func TestRunStopsStartedWorkersWhenALaterStartFails(t *testing.T) {
	fleet := &fakeFleet{startErr: map[string]error{"second": errors.New("poller exploded")}}
	h := newTestHost(t, Config{TaskQueues: []string{"first", "second"}}, fleet)

	err := h.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `task queue "second"`) {
		t.Fatalf("Run = %v, want the failing queue named", err)
	}
	if len(fleet.workers) != 2 {
		t.Fatalf("workers = %d", len(fleet.workers))
	}
	if !fleet.workers[0].stopped {
		t.Error("first worker leaked after the second failed to start")
	}
}

func TestRunPropagatesDialFailure(t *testing.T) {
	h, err := New(Config{TaskQueues: []string{"goobers-engine"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.dial = func(string, string) (client.Client, error) { return nil, errors.New("no frontend") }
	if err := h.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "dial temporal") {
		t.Fatalf("Run = %v, want dial failure", err)
	}
}

func TestDrainTimeoutDefaults(t *testing.T) {
	h, err := New(Config{TaskQueues: []string{"q"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := h.workerOptions().WorkerStopTimeout; got != DefaultDrainTimeout {
		t.Fatalf("WorkerStopTimeout = %v, want DefaultDrainTimeout", got)
	}
}

// fakeNextActivity asserts the tracker counts the execution window itself.
type fakeNextActivity struct {
	interceptor.ActivityInboundInterceptorBase
	tracker *activityTracker
	t       *testing.T
}

func (f *fakeNextActivity) ExecuteActivity(context.Context, *interceptor.ExecuteActivityInput) (interface{}, error) {
	if got := f.tracker.inFlight(); got != 1 {
		f.t.Errorf("in-flight during execution = %d, want 1", got)
	}
	return nil, nil
}

func TestActivityTrackerCountsExecutionWindow(t *testing.T) {
	tracker := &activityTracker{}
	next := &fakeNextActivity{tracker: tracker, t: t}
	inbound := tracker.InterceptActivity(context.Background(), next)
	if _, err := inbound.ExecuteActivity(context.Background(), &interceptor.ExecuteActivityInput{}); err != nil {
		t.Fatalf("ExecuteActivity: %v", err)
	}
	if got := tracker.inFlight(); got != 0 {
		t.Fatalf("in-flight after completion = %d, want 0", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached before deadline")
}
