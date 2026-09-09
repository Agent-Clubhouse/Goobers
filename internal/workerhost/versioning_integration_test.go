//go:build integration

package workerhost

import (
	"context"
	"errors"
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
	r.mu.Lock()
	r.attempts = append(r.attempts, versioningAttempt{
		Identity: identity,
		Attempt:  int(activity.GetInfo(ctx).Attempt),
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

func versioningWorkflow(ctx workflow.Context) ([]versioningActivityResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		ScheduleToStartTimeout: time.Second,
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
		ScheduleToStartTimeout: time.Second,
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

func startVersionedWorker(t *testing.T, c client.Client, queue, buildID, identity string, recorder *versioningRecorder) worker.Worker {
	t.Helper()
	tracker := &activityTracker{buildID: buildID, worker: identity}
	w := worker.New(c, queue, worker.Options{
		Identity:     identity,
		BuildID:      buildID,
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
	w.RegisterWorkflow(versioningWorkflow)
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
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()

	queue := "workerhost-versioning"
	oldRecords := &versioningRecorder{}
	newRecords := &versioningRecorder{}
	oldWorker := startVersionedWorker(t, server.Client(), queue, "build-old", "old-worker", oldRecords)
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "versioning-pinned",
		TaskQueue: queue,
	}, versioningWorkflow)
	if err != nil {
		t.Fatalf("start old workflow: %v", err)
	}

	waitFor(t, func() bool {
		attempts := oldRecords.snapshot()
		return len(attempts) == 2 && attempts[0].Failed && attempts[1].Identity.BuildID == "build-old"
	})
	oldWorker.Stop()
	startVersionedWorker(t, server.Client(), queue, "build-new", "new-worker", newRecords)

	probeRun, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "versioning-probe",
		TaskQueue: queue,
	}, versioningProbeWorkflow)
	if err != nil {
		t.Fatalf("start probe workflow: %v", err)
	}
	var probeResult versioningActivityResult
	if err := probeRun.Get(ctx, &probeResult); err != nil {
		t.Fatalf("probe workflow on new worker: %v", err)
	}
	if probeResult.BuildID != "build-new" || probeResult.WorkerIdentity != "new-worker" {
		t.Fatalf("probe workflow result = %+v, want build-new/new-worker", probeResult)
	}

	if err := server.Client().SignalWorkflow(ctx, run.GetID(), run.GetRunID(), "resume", nil); err != nil {
		t.Fatalf("signal old workflow: %v", err)
	}
	resultErr := make(chan error, 1)
	go func() { resultErr <- run.Get(ctx, nil) }()
	var historyErr error
	waitFor(t, func() bool {
		iter := server.Client().GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		scheduled := int64(0)
		for iter.HasNext() {
			event, err := iter.Next()
			if err != nil {
				historyErr = err
				return false
			}
			if event.GetEventType() == enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED {
				scheduled++
			}
			if scheduled >= 3 && event.GetEventType() == enumspb.EVENT_TYPE_ACTIVITY_TASK_TIMED_OUT {
				attrs := event.GetActivityTaskTimedOutEventAttributes()
				if attrs.GetStartedEventId() == 0 {
					return true
				}
			}
		}
		return false
	})
	if historyErr != nil {
		t.Fatalf("read pinned workflow history: %v", historyErr)
	}
	select {
	case err := <-resultErr:
		t.Fatalf("new worker executed workflow pinned to build-old: %v", err)
	default:
	}
	startVersionedWorker(t, server.Client(), queue, "build-old", "old-worker-resumed", oldRecords)
	var result []versioningActivityResult
	if err := run.Get(ctx, &result); err != nil {
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

	newRun, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "versioning-new",
		TaskQueue: queue,
	}, versioningWorkflow)
	if err != nil {
		t.Fatalf("start new workflow: %v", err)
	}
	_ = server.Client().SignalWorkflow(ctx, newRun.GetID(), newRun.GetRunID(), "resume", nil)
	if err := newRun.Get(ctx, nil); err != nil {
		t.Fatalf("new workflow: %v", err)
	}
	waitFor(t, func() bool {
		attempts := newRecords.snapshot()
		return len(attempts) >= 3
	})
	for _, attempt := range newRecords.snapshot() {
		if attempt.Identity.BuildID != "build-new" || attempt.Identity.WorkerIdentity != "new-worker" {
			t.Fatalf("new attempt identity = %+v, want build-new/new-worker", attempt)
		}
	}
}
