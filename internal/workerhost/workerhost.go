// Package workerhost hosts the Temporal engine worker behind `goobers worker`
// (#632, v2-cloud-scale A1.6): task-queue selection, graceful drain on
// shutdown, and a versioned worker identity, so tier-3 workers are a
// deployable unit (k8s-infra-shape §2). The engine itself stays quarantined —
// this package only makes it hostable; it adds no orchestration semantics.
package workerhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/bootstrap"
)

// ErrAbandonedWork reports a drain that expired with activities still in
// flight: the process exits distinctly so an orchestrator (or operator) can
// tell a clean rollout from one that cut work short.
var ErrAbandonedWork = errors.New("workerhost: drain timeout expired with activities still in flight")

// DefaultDrainTimeout bounds how long Stop waits for in-flight activities
// after a shutdown signal before abandoning them.
const DefaultDrainTimeout = 30 * time.Second

// Config describes one worker process.
type Config struct {
	// HostPort is the Temporal frontend address.
	HostPort string
	// Namespace is the Temporal namespace.
	Namespace string
	// TaskQueues are the queues this process serves — one Temporal worker per
	// queue, all registering the identical engine workflow/activity set.
	TaskQueues []string
	// DrainTimeout bounds graceful drain (worker.Options.WorkerStopTimeout).
	// Zero applies DefaultDrainTimeout.
	DrainTimeout time.Duration
	// BuildVersion is stamped into the worker identity so Temporal visibility
	// alone answers "which build serves this queue".
	BuildVersion string
	// Deps are the engine execution seams registered on every worker.
	Deps bootstrap.EngineDeps
}

// managedWorker is the slice of worker.Worker the host drives; tests fake it.
type managedWorker interface {
	Start() error
	Stop()
}

// Host runs one configured worker process.
type Host struct {
	cfg     Config
	tracker *activityTracker

	// Seams for hermetic tests: dialing Temporal and constructing one
	// registered worker per queue.
	dial      func(hostPort, namespace string) (client.Client, error)
	newWorker func(c client.Client, taskQueue string, opts worker.Options) managedWorker
}

// New validates cfg and builds a Host.
func New(cfg Config) (*Host, error) {
	if len(cfg.TaskQueues) == 0 {
		return nil, errors.New("workerhost: at least one task queue is required")
	}
	for _, q := range cfg.TaskQueues {
		if q == "" {
			return nil, errors.New("workerhost: task queue names must be non-empty")
		}
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	h := &Host{cfg: cfg, tracker: &activityTracker{}}
	h.dial = bootstrap.DialTemporal
	h.newWorker = func(c client.Client, taskQueue string, opts worker.Options) managedWorker {
		w := worker.New(c, taskQueue, opts)
		bootstrap.RegisterEngine(w, cfg.Deps)
		return w
	}
	return h, nil
}

// Identity is the versioned Temporal worker identity (#632): build version,
// host, and pid, so mid-run compatibility questions are diagnosable from
// Temporal visibility alone.
func Identity(buildVersion string) string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	return fmt.Sprintf("goobers-worker/%s@%s#%d", buildVersion, host, os.Getpid())
}

// workerOptions builds the one options set every queue's worker runs under.
func (h *Host) workerOptions() worker.Options {
	return worker.Options{
		Identity:          Identity(h.cfg.BuildVersion),
		WorkerStopTimeout: h.cfg.DrainTimeout,
		Interceptors:      []interceptor.WorkerInterceptor{h.tracker},
	}
}

// Run serves the configured task queues until ctx is cancelled (SIGTERM/
// SIGINT via the caller's signal context), then drains: workers stop polling
// and in-flight activities get up to DrainTimeout to complete. Returns nil on
// a clean drain and ErrAbandonedWork when work was cut short.
func (h *Host) Run(ctx context.Context) error {
	c, err := h.dial(h.cfg.HostPort, h.cfg.Namespace)
	if err != nil {
		return fmt.Errorf("workerhost: dial temporal %s (namespace %s): %w", h.cfg.HostPort, h.cfg.Namespace, err)
	}
	if c != nil {
		defer c.Close()
	}

	opts := h.workerOptions()
	started := make([]managedWorker, 0, len(h.cfg.TaskQueues))
	stopAll := func() {
		// Stop in reverse start order; each Stop honors WorkerStopTimeout.
		for i := len(started) - 1; i >= 0; i-- {
			started[i].Stop()
		}
	}
	for _, queue := range h.cfg.TaskQueues {
		w := h.newWorker(c, queue, opts)
		if err := w.Start(); err != nil {
			stopAll()
			return fmt.Errorf("workerhost: start worker for task queue %q: %w", queue, err)
		}
		started = append(started, w)
	}

	<-ctx.Done()
	stopAll()
	if n := h.tracker.inFlight(); n > 0 {
		return fmt.Errorf("%w: %d abandoned", ErrAbandonedWork, n)
	}
	return nil
}

// activityTracker counts in-flight activity executions across every worker in
// the process, via the SDK's worker interceptor chain. After Stop returns,
// a non-zero count is work the drain window abandoned.
type activityTracker struct {
	interceptor.WorkerInterceptorBase
	n atomic.Int64
}

func (t *activityTracker) inFlight() int64 { return t.n.Load() }

func (t *activityTracker) InterceptActivity(_ context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &trackedActivityInbound{
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next},
		tracker:                        t,
	}
}

type trackedActivityInbound struct {
	interceptor.ActivityInboundInterceptorBase
	tracker *activityTracker
}

func (a *trackedActivityInbound) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (interface{}, error) {
	a.tracker.n.Add(1)
	defer a.tracker.n.Add(-1)
	return a.Next.ExecuteActivity(ctx, in)
}
