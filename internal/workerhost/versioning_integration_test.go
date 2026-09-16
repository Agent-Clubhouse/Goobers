//go:build integration

package workerhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/attemptidentity"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type versioningAttempt struct {
	Identity attemptidentity.Identity
	Workflow string
	Attempt  int
	Failed   bool
}

type versioningRecorder struct {
	mu       sync.Mutex
	attempts []versioningAttempt
}

func (r *versioningRecorder) record(ctx context.Context, failed bool) {
	identity, ok := attemptidentity.FromContext(ctx)
	if !ok {
		return
	}
	info := activity.GetInfo(ctx)
	r.mu.Lock()
	r.attempts = append(r.attempts, versioningAttempt{
		Identity: identity,
		Workflow: info.WorkflowExecution.ID,
		Attempt:  int(info.Attempt),
		Failed:   failed,
	})
	r.mu.Unlock()
}

func (r *versioningRecorder) snapshot() []versioningAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]versioningAttempt(nil), r.attempts...)
}

type versioningActivityResult struct {
	BuildID        string
	WorkerIdentity string
	Attempt        int
}

const versioningScheduleToStartTimeout = 5 * time.Second

func waitForVersioning(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached before deadline")
}

func versioningWorkflow(ctx workflow.Context) ([]versioningActivityResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		ScheduleToStartTimeout: versioningScheduleToStartTimeout,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 2},
	})
	var first versioningActivityResult
	if err := workflow.ExecuteActivity(ctx, "VersioningActivity", true).Get(ctx, &first); err != nil {
		return nil, err
	}
	var resume struct{}
	workflow.GetSignalChannel(ctx, "resume").Receive(ctx, &resume)
	var resumed versioningActivityResult
	if err := workflow.ExecuteActivity(ctx, "VersioningActivity", false).Get(ctx, &resumed); err != nil {
		return nil, err
	}
	return []versioningActivityResult{first, resumed}, nil
}

func versioningProbeWorkflow(ctx workflow.Context) (versioningActivityResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		ScheduleToStartTimeout: versioningScheduleToStartTimeout,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var result versioningActivityResult
	if err := workflow.ExecuteActivity(ctx, "VersioningActivity", false).Get(ctx, &result); err != nil {
		return versioningActivityResult{}, err
	}
	return result, nil
}

func versioningActivity(recorder *versioningRecorder) func(context.Context, bool) (versioningActivityResult, error) {
	return func(ctx context.Context, failFirst bool) (versioningActivityResult, error) {
		identity, ok := attemptidentity.FromContext(ctx)
		if !ok {
			return versioningActivityResult{}, errors.New("activity execution identity missing")
		}
		attempt := activity.GetInfo(ctx).Attempt
		if failFirst && attempt == 1 {
			recorder.record(ctx, true)
			return versioningActivityResult{}, temporal.NewApplicationError("simulated failed attempt", "VersioningAttemptFailed")
		}
		recorder.record(ctx, false)
		return versioningActivityResult{
			BuildID:        identity.BuildID,
			WorkerIdentity: identity.WorkerIdentity,
			Attempt:        int(attempt),
		}, nil
	}
}

func waitForProbe(t *testing.T, ctx context.Context, c client.Client, recorder *versioningRecorder, opts client.StartWorkflowOptions, want versioningActivityResult) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		tryOpts := opts
		tryOpts.ID = fmt.Sprintf("%s-%d", opts.ID, attempt)
		if _, err := c.ExecuteWorkflow(ctx, tryOpts, "versioningProbeWorkflow"); err != nil {
			t.Fatalf("start probe workflow %q attempt %d: %v", opts.ID, attempt, err)
		}
		probeDeadline := time.Now().Add(versioningScheduleToStartTimeout + 2*time.Second)
		for time.Now().Before(probeDeadline) {
			for _, recorded := range recorder.snapshot() {
				if recorded.Workflow != tryOpts.ID {
					continue
				}
				if recorded.Failed {
					t.Fatalf("probe workflow %q attempt %d recorded a failed activity attempt: %+v", opts.ID, attempt, recorded)
				}
				got := versioningActivityResult{
					BuildID:        recorded.Identity.BuildID,
					WorkerIdentity: recorded.Identity.WorkerIdentity,
					Attempt:        recorded.Attempt,
				}
				if got != want {
					t.Fatalf("probe workflow %q = %+v, want %+v", opts.ID, got, want)
				}
				return
			}
			if ctx.Err() != nil {
				t.Fatalf("probe workflow %q: %v", opts.ID, ctx.Err())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Fatalf("probe workflow %q did not reach %+v before deadline", opts.ID, want)
}

func waitForCurrentVersion(t *testing.T, ctx context.Context, handle client.WorkerDeploymentHandle, buildID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := handle.SetCurrentVersion(callCtx, client.WorkerDeploymentSetCurrentVersionOptions{BuildID: buildID})
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("set current build ID %q: %v", buildID, lastErr)
}

func startVersionedWorker(t *testing.T, c client.Client, queue, buildID, identity string, recorder *versioningRecorder) worker.Worker {
	t.Helper()
	tracker := &activityTracker{buildID: buildID, worker: identity}
	w := worker.New(c, queue, worker.Options{
		Identity:     identity,
		Interceptors: []interceptor.WorkerInterceptor{tracker},
		DeploymentOptions: worker.DeploymentOptions{
			UseVersioning: true,
			Version: worker.WorkerDeploymentVersion{
				DeploymentName: "goobers",
				BuildID:        buildID,
			},
			DefaultVersioningBehavior: workflow.VersioningBehaviorPinned,
		},
	})
	w.RegisterWorkflowWithOptions(versioningWorkflow, workflow.RegisterOptions{
		VersioningBehavior: workflow.VersioningBehaviorPinned,
	})
	w.RegisterWorkflowWithOptions(versioningProbeWorkflow, workflow.RegisterOptions{
		Name:               "versioningProbeWorkflow",
		VersioningBehavior: workflow.VersioningBehaviorAutoUpgrade,
	})
	w.RegisterActivityWithOptions(versioningActivity(recorder), activity.RegisterOptions{Name: "VersioningActivity"})
	if err := w.Start(); err != nil {
		t.Fatalf("start %s worker: %v", buildID, err)
	}
	t.Cleanup(w.Stop)
	return w
}

func TestIntegrationTemporalVersioningPinsMixedFleetAndRecordsAttempts(t *testing.T) {
	testdep.RequireEnv(t, temporaltest.CLIEnvVar)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		ExtraArgs: []string{
			"--dynamic-config-value", "frontend.enableWorkerVersioning=true",
			"--dynamic-config-value", "system.enableWorkerVersioning=true",
			"--dynamic-config-value", "frontend.workerVersioningDataAPIs=true",
			"--dynamic-config-value", "frontend.workerVersioningRuleAPIs=true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()

	queue := "workerhost-versioning"
	oldRecords := &versioningRecorder{}
	newRecords := &versioningRecorder{}
	oldWorker := startVersionedWorker(t, server.Client(), queue, "build-old", "old-worker", oldRecords)
	deployment := server.Client().WorkerDeploymentClient().GetHandle("goobers")
	waitForCurrentVersion(t, ctx, deployment, "build-old")
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:               "versioning-pinned",
		TaskQueue:        queue,
		EnableEagerStart: true,
	}, versioningWorkflow)
	if err != nil {
		t.Fatalf("start old workflow: %v", err)
	}

	waitForVersioning(t, func() bool {
		attempts := oldRecords.snapshot()
		return len(attempts) == 2 && attempts[0].Failed && attempts[1].Identity.BuildID == "build-old"
	})
	oldWorker.Stop()
	startVersionedWorker(t, server.Client(), queue, "build-new", "new-worker", newRecords)
	waitForCurrentVersion(t, ctx, deployment, "build-new")
	waitForProbe(t, ctx, server.Client(), newRecords, client.StartWorkflowOptions{
		ID:                 "versioning-probe",
		TaskQueue:          queue,
		VersioningOverride: &client.AutoUpgradeVersioningOverride{},
	}, versioningActivityResult{BuildID: "build-new", WorkerIdentity: "new-worker", Attempt: 1})

	if err := server.Client().SignalWorkflow(ctx, run.GetID(), run.GetRunID(), "resume", nil); err != nil {
		t.Fatalf("signal old workflow: %v", err)
	}
	resultErr := make(chan error, 1)
	go func() { resultErr <- run.Get(ctx, nil) }()
	waitForProbe(t, ctx, server.Client(), newRecords, client.StartWorkflowOptions{
		ID:                 "versioning-probe-after-signal",
		TaskQueue:          queue,
		VersioningOverride: &client.AutoUpgradeVersioningOverride{},
	}, versioningActivityResult{BuildID: "build-new", WorkerIdentity: "new-worker", Attempt: 1})
	var (
		historyErr          error
		signalRecorded      bool
		taskScheduled       bool
		taskStartedOnNewSet bool
	)
	waitForVersioning(t, func() bool {
		iter := server.Client().GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		var signalEventID int64
		var scheduledEventID int64
		signalRecorded, taskScheduled, taskStartedOnNewSet = false, false, false
		for iter.HasNext() {
			event, err := iter.Next()
			if err != nil {
				historyErr = err
				return false
			}
			switch event.GetEventType() {
			case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED:
				signalRecorded = true
				signalEventID = event.GetEventId()
			case enumspb.EVENT_TYPE_WORKFLOW_TASK_SCHEDULED:
				if signalEventID > 0 && event.GetEventId() > signalEventID {
					taskScheduled = true
					scheduledEventID = event.GetEventId()
				}
			case enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED:
				attrs := event.GetWorkflowTaskStartedEventAttributes()
				if scheduledEventID > 0 && attrs.GetScheduledEventId() == scheduledEventID {
					taskStartedOnNewSet = true
					return true
				}
			}
		}
		return signalRecorded && taskScheduled
	})
	if historyErr != nil {
		t.Fatalf("read pinned workflow history: %v", historyErr)
	}
	if taskStartedOnNewSet {
		t.Fatal("new worker started workflow work for the build-old pinned execution during the new-worker-only window")
	}
	select {
	case err := <-resultErr:
		t.Fatalf("new worker executed workflow pinned to build-old: %v", err)
	default:
	}
	startVersionedWorker(t, server.Client(), queue, "build-old", "old-worker-resumed", oldRecords)
	waitForProbe(t, ctx, server.Client(), oldRecords, client.StartWorkflowOptions{
		ID:        "versioning-old-ready",
		TaskQueue: queue,
		VersioningOverride: &client.PinnedVersioningOverride{Version: worker.WorkerDeploymentVersion{
			DeploymentName: "goobers",
			BuildID:        "build-old",
		}},
	}, versioningActivityResult{BuildID: "build-old", WorkerIdentity: "old-worker-resumed", Attempt: 1})
	var result []versioningActivityResult
	resultCtx, resultCancel := context.WithTimeout(ctx, 15*time.Second)
	defer resultCancel()
	if err := run.Get(resultCtx, &result); err != nil {
		t.Fatalf("old workflow: %v", err)
	}
	if len(result) != 2 || result[0].BuildID != "build-old" || result[1].BuildID != "build-old" {
		t.Fatalf("pinned workflow activities = %+v, want both build-old", result)
	}
	for _, attempt := range oldRecords.snapshot() {
		if attempt.Identity.BuildID != "build-old" || attempt.Identity.WorkerIdentity == "" {
			t.Fatalf("old attempt identity = %+v, want build-old and a worker identity", attempt)
		}
	}
	if attempts := newRecords.snapshot(); len(attempts) != 2 ||
		attempts[0].Identity.BuildID != "build-new" || attempts[0].Identity.WorkerIdentity != "new-worker" || attempts[0].Failed ||
		attempts[1].Identity.BuildID != "build-new" || attempts[1].Identity.WorkerIdentity != "new-worker" || attempts[1].Failed {
		t.Fatalf("new build attempts = %+v, want exactly the successful unpinned probe activities on build-new/new-worker", attempts)
	}
}
