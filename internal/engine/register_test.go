package engine

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus-rpc/sdk-go/nexus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/temporaltest"
)

type recordingWorker struct {
	workflowOpts []workflow.RegisterOptions
}

func (w *recordingWorker) RegisterWorkflow(interface{}) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterWorkflowWithOptions(_ interface{}, opts workflow.RegisterOptions) {
	w.workflowOpts = append(w.workflowOpts, opts)
}

func (w *recordingWorker) RegisterDynamicWorkflow(interface{}, workflow.DynamicRegisterOptions) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterActivity(interface{}) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterActivityWithOptions(interface{}, activity.RegisterOptions) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterDynamicActivity(interface{}, activity.DynamicRegisterOptions) {
	// Unused in this test.
}

func (w *recordingWorker) RegisterNexusService(*nexus.Service) {
	// Unused in this test.
}

func (w *recordingWorker) Start() error { return nil }

func (w *recordingWorker) Stop() {}

func (w *recordingWorker) Run(<-chan interface{}) error { return nil }

func TestRegisterWithPinsWorkflowsToCurrentBuild(t *testing.T) {
	w := &recordingWorker{}
	RegisterWith(w, &Activities{})

	if got := len(w.workflowOpts); got != 5 {
		t.Fatalf("registered workflows = %d, want 5", got)
	}
	for i, opts := range w.workflowOpts {
		if got := opts.VersioningBehavior; got != workflow.VersioningBehaviorPinned {
			t.Fatalf("workflow %d versioning behavior = %v, want %v", i, got, workflow.VersioningBehaviorPinned)
		}
	}
}

const (
	versioningSignal   = "resume"
	versioningActivity = "versioningActivity"
)

var (
	oldVersioningActivityCount atomic.Int32
	newVersioningActivityCount atomic.Int32
)

func oldPinnedVersioningWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{
		StartToCloseTimeout: time.Second,
	})
	var started string
	if err := workflow.ExecuteLocalActivity(ctx, pinnedBuildActivity).Get(ctx, &started); err != nil {
		return "", err
	}
	var signal struct{}
	workflow.GetSignalChannel(ctx, versioningSignal).Receive(ctx, &signal)
	return "old-build", nil
}

func newPinnedVersioningWorkflow(ctx workflow.Context) (string, error) {
	var signal struct{}
	workflow.GetSignalChannel(ctx, versioningSignal).Receive(ctx, &signal)
	return "new-build", nil
}

func pinnedBuildActivity(context.Context) (string, error) {
	return "started", nil
}

func unpinnedProbeWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Second,
	})
	var build string
	if err := workflow.ExecuteActivity(ctx, versioningActivity).Get(ctx, &build); err != nil {
		return "", err
	}
	return build, nil
}

func oldVersioningActivity(context.Context) (string, error) {
	oldVersioningActivityCount.Add(1)
	return "old-build", nil
}

func newVersioningActivity(context.Context) (string, error) {
	newVersioningActivityCount.Add(1)
	return "new-build", nil
}

func waitForVersioningCount(t *testing.T, count *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if count.Load() >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("activity count = %d, want at least %d", count.Load(), want)
}

func registerVersioningWorker(t *testing.T, c client.Client, queue, build string, workflowFn, activityFn interface{}) worker.Worker {
	t.Helper()
	w := worker.New(c, queue, worker.Options{
		BuildID:                 build,
		UseBuildIDForVersioning: true,
	})
	w.RegisterWorkflowWithOptions(workflowFn, workflow.RegisterOptions{
		VersioningBehavior: workflow.VersioningBehaviorPinned,
	})
	w.RegisterWorkflow(unpinnedProbeWorkflow)
	w.RegisterActivity(pinnedBuildActivity)
	w.RegisterActivityWithOptions(activityFn, activity.RegisterOptions{Name: versioningActivity})
	if err := w.Start(); err != nil {
		t.Fatalf("start %s worker: %v", build, err)
	}
	return w
}

func TestVersionedWorkersPinLongRunningWorkflowAcrossResume(t *testing.T) {
	startCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(startCtx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		ClientOptions: &client.Options{
			Namespace: "versioning-test",
		},
		ExtraArgs: []string{
			"--dynamic-config-value", "frontend.enableWorkerVersioning=true",
			"--dynamic-config-value", "system.enableWorkerVersioning=true",
			"--dynamic-config-value", "frontend.workerVersioningDataAPIs=true",
			"--dynamic-config-value", "frontend.workerVersioningRuleAPIs=true",
		},
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})

	const queue = "goobers-worker-versioning"
	c := server.Client()
	oldVersioningActivityCount.Store(0)
	newVersioningActivityCount.Store(0)
	oldWorker := registerVersioningWorker(t, c, queue, "old-build", oldPinnedVersioningWorkflow, oldVersioningActivity)
	versioningCtx, versioningCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := c.UpdateWorkerBuildIdCompatibility(versioningCtx, &client.UpdateWorkerBuildIdCompatibilityOptions{
		TaskQueue: queue,
		Operation: &client.BuildIDOpAddNewIDInNewDefaultSet{BuildID: "old-build"},
	}); err != nil {
		versioningCancel()
		oldWorker.Stop()
		t.Fatalf("register old build ID: %v", err)
	}
	versioningCancel()
	oldProbe, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID: fmt.Sprintf("old-probe-%d", time.Now().UnixNano()), TaskQueue: queue,
	}, "unpinnedProbeWorkflow")
	if err != nil {
		oldWorker.Stop()
		t.Fatalf("start old-build readiness probe: %v", err)
	}
	var oldProbeBuild string
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer probeCancel()
	if err := oldProbe.Get(probeCtx, &oldProbeBuild); err != nil {
		oldWorker.Stop()
		t.Fatalf("get old-build readiness probe: %v", err)
	}
	if oldProbeBuild != "old-build" {
		oldWorker.Stop()
		t.Fatalf("old-build readiness probe = %q, want old-build", oldProbeBuild)
	}
	waitForVersioningCount(t, &oldVersioningActivityCount, 1)
	workflowID := fmt.Sprintf("versioning-%d", time.Now().UnixNano())
	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID: workflowID, TaskQueue: queue, EnableEagerStart: true,
	}, oldPinnedVersioningWorkflow)
	if err != nil {
		oldWorker.Stop()
		t.Fatalf("start pinned workflow: %v", err)
	}
	time.Sleep(time.Second)
	initialCtx, initialCancel := context.WithTimeout(context.Background(), time.Second)
	var initialResult string
	if err := run.Get(initialCtx, &initialResult); err == nil {
		t.Fatalf("pinned workflow completed before resume with result %q", initialResult)
	}
	initialCancel()

	oldWorker.Stop()
	versioningCtx, versioningCancel = context.WithTimeout(context.Background(), 10*time.Second)
	if err := c.UpdateWorkerBuildIdCompatibility(versioningCtx, &client.UpdateWorkerBuildIdCompatibilityOptions{
		TaskQueue: queue,
		Operation: &client.BuildIDOpAddNewIDInNewDefaultSet{BuildID: "new-build"},
	}); err != nil {
		versioningCancel()
		t.Fatalf("register new build ID: %v", err)
	}
	versioningCancel()
	newWorker := registerVersioningWorker(t, c, queue, "new-build", newPinnedVersioningWorkflow, newVersioningActivity)
	defer newWorker.Stop()

	probeRun, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID: fmt.Sprintf("probe-%d", time.Now().UnixNano()), TaskQueue: queue,
	}, "unpinnedProbeWorkflow")
	if err != nil {
		t.Fatalf("start unpinned probe: %v", err)
	}
	var probeBuild string
	if err := probeRun.Get(context.Background(), &probeBuild); err != nil {
		t.Fatalf("get unpinned probe: %v", err)
	}
	if probeBuild != "new-build" {
		t.Fatalf("probe build = %q, want new-build", probeBuild)
	}
	if got := newVersioningActivityCount.Load(); got != 1 {
		t.Fatalf("new-build probe activity count = %d, want 1", got)
	}

	if err := c.SignalWorkflow(context.Background(), workflowID, "", versioningSignal, struct{}{}); err != nil {
		t.Fatalf("signal pinned workflow: %v", err)
	}

	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	var result string
	err = run.Get(resumeCtx, &result)
	resumeCancel()
	if err == nil {
		t.Fatalf("pinned workflow completed on new-only fleet with result %q and error %v", result, err)
	}
	if got := newVersioningActivityCount.Load(); got != 1 {
		t.Fatalf("new-only activity count = %d, want only the probe", got)
	}

	oldWorker = registerVersioningWorker(t, c, queue, "old-build", oldPinnedVersioningWorkflow, oldVersioningActivity)
	defer oldWorker.Stop()
	if err := run.Get(context.Background(), &result); err != nil {
		t.Fatalf("get resumed pinned workflow: %v", err)
	}
	if result != "old-build" {
		t.Fatalf("pinned workflow result = %q, want old-build", result)
	}
}
