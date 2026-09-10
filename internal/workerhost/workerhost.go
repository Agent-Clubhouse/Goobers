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
	"sync"
	"sync/atomic"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/attemptidentity"
	"github.com/goobers/goobers/internal/bootstrap"
)

// ErrAbandonedWork reports a drain that expired with activities still in
// flight: the process exits distinctly so an orchestrator (or operator) can
// tell a clean rollout from one that cut work short.
var ErrAbandonedWork = errors.New("workerhost: drain timeout expired with activities still in flight")

// DefaultDrainTimeout bounds how long Stop waits for in-flight activities
// after a shutdown signal before abandoning them.
const DefaultDrainTimeout = 30 * time.Second

const (
	placementBuildEnv  = "GOOBERS_RUNNER_BUILD"
	placementWorkerEnv = "GOOBERS_RUNNER_WORKER"
)

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
	h := &Host{cfg: cfg, tracker: &activityTracker{
		buildID: cfg.BuildVersion,
		worker:  Identity(cfg.BuildVersion),
	}}
	h.dial = bootstrap.DialTemporal
	h.newWorker = func(c client.Client, taskQueue string, opts worker.Options) managedWorker {
		w := worker.New(c, taskQueue, opts)
		bootstrap.RegisterEngine(w, c, cfg.Deps)
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
	opts := worker.Options{
		Identity:          Identity(h.cfg.BuildVersion),
		WorkerStopTimeout: h.cfg.DrainTimeout,
		Interceptors:      []interceptor.WorkerInterceptor{h.tracker},
	}
	if h.cfg.BuildVersion == "" {
		return opts
	}
	opts.BuildID = h.cfg.BuildVersion
	opts.UseBuildIDForVersioning = true
	opts.DeploymentOptions = worker.DeploymentOptions{
		UseVersioning: true,
		Version: worker.WorkerDeploymentVersion{
			DeploymentName: "goobers",
			BuildID:        h.cfg.BuildVersion,
		},
		DefaultVersioningBehavior: workflow.VersioningBehaviorPinned,
	}
	return opts
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

	previousBuild, hadBuild := os.LookupEnv(placementBuildEnv)
	previousWorker, hadWorker := os.LookupEnv(placementWorkerEnv)
	if h.cfg.BuildVersion != "" {
		_ = os.Setenv(placementBuildEnv, h.cfg.BuildVersion)
		_ = os.Setenv(placementWorkerEnv, Identity(h.cfg.BuildVersion))
	}
	defer func() {
		if hadBuild {
			_ = os.Setenv(placementBuildEnv, previousBuild)
		} else {
			_ = os.Unsetenv(placementBuildEnv)
		}
		if hadWorker {
			_ = os.Setenv(placementWorkerEnv, previousWorker)
		} else {
			_ = os.Unsetenv(placementWorkerEnv)
		}
	}()

	opts := h.workerOptions()
	started := make([]managedWorker, 0, len(h.cfg.TaskQueues))
	stopAll := func() {
		// Stop polling every queue immediately. Serial Stop calls would keep
		// later queues accepting work and multiply the process drain window.
		var draining sync.WaitGroup
		for _, w := range started {
			w := w
			draining.Add(1)
			go func() {
				defer draining.Done()
				w.Stop()
			}()
		}
		draining.Wait()
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
	n       atomic.Int64
	buildID string
	worker  string
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

// The SDK panics if GetInfo runs outside a real activity context; unit tests
// drive this interceptor directly with a background context.
func currentActivityInfo(ctx context.Context) (_ activity.Info, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return activity.GetInfo(ctx), true
}

func (a *trackedActivityInbound) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (interface{}, error) {
	a.tracker.n.Add(1)
	defer a.tracker.n.Add(-1)
	identity := attemptidentity.Identity{
		BuildID:        a.tracker.buildID,
		WorkerIdentity: a.tracker.worker,
	}
	if info, ok := currentActivityInfo(ctx); ok {
		identity.TaskQueue = info.TaskQueue
		identity.ActivityID = info.ActivityID
		identity.ActivityType = info.ActivityType.Name
		identity.Attempt = info.Attempt
	}
	ctx = attemptidentity.WithContext(ctx, identity)
	result, err := a.Next.ExecuteActivity(ctx, in)
	if err == nil {
		return result, nil
	}
	if temporal.IsCanceledError(err) || temporal.IsTerminatedError(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	// Keep the original error as the cause so its details and retry options
	// remain available to the engine, while the outer error carries identity.
	failureType := "GoobersAttemptFailure"
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		failureType = appErr.Type()
	}
	return nil, temporal.NewApplicationErrorWithOptions(err.Error(), failureType, temporal.ApplicationErrorOptions{
		NonRetryable: appErr != nil && appErr.NonRetryable(),
		Cause:        err,
		NextRetryDelay: func() time.Duration {
			if appErr == nil {
				return 0
			}
			return appErr.NextRetryDelay()
		}(),
		Category: func() temporal.ApplicationErrorCategory {
			if appErr == nil {
				return temporal.ApplicationErrorCategoryUnspecified
			}
			return appErr.Category()
		}(),
		Details: []interface{}{identity},
	})
}
