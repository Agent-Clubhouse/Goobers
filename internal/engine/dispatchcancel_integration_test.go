//go:build integration

package engine

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// A real SDK worker/server is required: the SDK's in-process workflow suite
// immediately completes canceled activity handles, even with WaitForCancellation.
// Require an explicitly provisioned CLI; this test never downloads a tool.
func cancellationDevServer(t *testing.T) (context.Context, *testsuite.DevServer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = server.Stop() })
	return ctx, server
}

// DELETE is accepted while the pod still exists, exactly as during Kubernetes
// termination grace. Only the test's observation gate removes the actual object.
func cancellationPods(t *testing.T) (*fake.Clientset, <-chan string, <-chan struct{}) {
	t.Helper()
	api := fake.NewSimpleClientset()
	created := make(chan string, 1)
	deleted := make(chan struct{}, 1)
	api.PrependReactor("create", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
		pod := action.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		created <- pod.Name
		return false, nil, nil
	})
	api.PrependReactor("delete", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		select {
		case deleted <- struct{}{}:
		default:
		}
		return true, nil, nil
	})
	return api, created, deleted
}

func TestIntegrationDispatchCancellationWaitsForPodCleanup(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, server := cancellationDevServer(t)
	for _, mode := range []string{"task", "one"} {
		t.Run(mode, func(t *testing.T) { assertDispatchCancellationCleanup(t, ctx, server, mode) })
	}
}

func assertDispatchCancellationCleanup(t *testing.T, ctx context.Context, server *testsuite.DevServer, mode string) {
	t.Helper()
	api, created, deleted := cancellationPods(t)
	store := surrenderStore(t)
	dispatch, err := dispatcher.New(dispatcher.Config{Namespace: "test", EmbeddedVersion: "v1", SupervisionInterval: time.Millisecond},
		dispatcher.NewKubernetesPodAPI(api), nil, dispatcher.PlaneSurrenderGate{Plane: store}, nil)
	if err != nil {
		t.Fatal(err)
	}
	queue := "dispatch-cancellation-" + mode
	workerCtx, stopActivities := context.WithCancel(ctx)
	w := temporalworker.New(server.Client(), queue, temporalworker.Options{BackgroundActivityContext: workerCtx, WorkerStopTimeout: time.Second})
	RegisterWith(w, &Activities{Workspaces: testWorkspaces(t), Dispatcher: dispatch, Surrenders: store})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopActivities(); w.Stop() })
	run := startCancellationWorkflow(t, ctx, server.Client(), queue, mode)
	var pod string
	select {
	case pod = <-created:
	case <-time.After(15 * time.Second):
		t.Fatal("dispatch did not create a pod")
	}
	t.Cleanup(func() {
		_ = api.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "test", pod)
	})
	if err := server.Client().CancelWorkflow(ctx, run.GetID(), run.GetRunID()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-deleted:
	case <-time.After(15 * time.Second):
		t.Fatal("Temporal cancellation never reached dispatched pod cleanup")
	}
	// Hold cleanup beyond the heartbeat timeout. Continued heartbeats must keep
	// the activity alive, and the canceled future must still await its result.
	waitCtx, stopWait := context.WithTimeout(ctx, dispatchHeartbeatTimeout+2*time.Second)
	err = run.Get(waitCtx, nil)
	waitErr := waitCtx.Err()
	stopWait()
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("workflow settled before pod disappearance: %v", err)
	}
	description, err := server.Client().DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
	if err != nil {
		t.Fatal(err)
	}
	if description.WorkflowExecutionInfo.Status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("workflow settled during cleanup: %v", description.WorkflowExecutionInfo.Status)
	}
	if mode == "task" {
		assertCancellationJournal(t, ctx, server.Client(), run, false)
	}
	if err := api.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "test", pod); err != nil {
		t.Fatal(err)
	}
	if err := run.Get(ctx, nil); !temporal.IsCanceledError(err) {
		t.Fatalf("settled cancellation = %v", err)
	}
	if mode == "task" {
		assertCancellationJournal(t, ctx, server.Client(), run, true)
	}
	history := cancellationHistory(t, ctx, server.Client(), run)
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	replayer.RegisterWorkflow(DispatchOne)
	if err := replayer.ReplayWorkflowHistoryWithOptions(nil, history, temporalworker.ReplayWorkflowHistoryOptions{OriginalExecution: workflow.Execution{ID: run.GetID(), RunID: run.GetRunID()}}); err != nil {
		t.Fatalf("replay canceled dispatch: %v", err)
	}
}

func startCancellationWorkflow(t *testing.T, ctx context.Context, c client.Client, queue, mode string) client.WorkflowRun {
	t.Helper()
	spec := apiv1.WorkflowSpec{Gaggle: "web", Start: "build", Tasks: []apiv1.Task{{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build", Run: &apiv1.DeterministicRun{Command: []string{"sleep", "45"}, Workspace: apiv1.WorkspaceScratch}}}}
	in := runInput("dispatch-cancellation-"+mode, spec)
	in.Placements = []PinnedPlacement{{Stage: "build", Queue: queue, Eligible: remoteEligible()}}
	var target any = Run
	var payload any = in
	id := in.RunID
	if mode == "one" {
		attempt := dispatchInput(in.RunID, "build", 1)
		attempt.Placement.Queue = queue
		attempt.Placement.LedgerTouching = false
		attempt.Run = spec.Tasks[0].Run
		target, payload = DispatchOne, attempt
		id = DispatchOneWorkflowID(in.RunID, "build", 1)
	}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: queue}, target, payload)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func cancellationHistory(t *testing.T, ctx context.Context, c client.Client, run client.WorkflowRun) *historypb.History {
	t.Helper()
	history := &historypb.History{}
	iter := c.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		history.Events = append(history.Events, event)
	}
	return history
}

// This is the pre-change DispatchOne command sequence, recorded by a real
// server. Replaying it with current DispatchOne must select DefaultVersion.
func legacyCancellationDispatchOne(ctx workflow.Context, in DispatchStageInput) (DispatchStageResult, error) {
	ctx = workflow.WithActivityOptions(ctx, stageActivityOptions(in.Envelope.Limits, in.Placement.Queue))
	in.OwningWorkflowID = workflow.GetInfo(ctx).WorkflowExecution.ID
	var result DispatchStageResult
	err := workflow.ExecuteActivity(ctx, ActDispatchStage, in).Get(ctx, &result)
	return result, err
}

func TestIntegrationDispatchCancellationReplaysOldOptions(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, server := cancellationDevServer(t)
	const queue = "dispatch-cancellation-legacy"
	store := surrenderStore(t)
	in := dispatchInput("dispatch-legacy", "build", 1)
	in.Placement.Queue = queue
	in.Run = &apiv1.DeterministicRun{Command: []string{"build"}, Workspace: apiv1.WorkspaceScratch}
	putSurrendered(t, store, in.Envelope.RunID, "build", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	w := temporalworker.New(server.Client(), queue, temporalworker.Options{})
	w.RegisterWorkflowWithOptions(legacyCancellationDispatchOne, workflow.RegisterOptions{Name: "DispatchOne"})
	w.RegisterActivity(&Activities{Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{SurrenderConfirmed: true}}, Surrenders: store})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "dispatch-legacy/build/1", TaskQueue: queue}, "DispatchOne", in)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	history := cancellationHistory(t, ctx, server.Client(), run)
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(DispatchOne)
	if err := replayer.ReplayWorkflowHistoryWithOptions(nil, history, temporalworker.ReplayWorkflowHistoryOptions{OriginalExecution: workflow.Execution{ID: run.GetID(), RunID: run.GetRunID()}}); err != nil {
		t.Fatalf("replay legacy options: %v", err)
	}
}

// Public cancellation acknowledges a request; the actual run journal must not
// claim terminal cancellation before the dispatched activity finishes cleanup.
func assertCancellationJournal(t *testing.T, ctx context.Context, c client.Client, run client.WorkflowRun, settled bool) {
	t.Helper()
	value, err := c.QueryWorkflow(ctx, run.GetID(), run.GetRunID(), JournalQuery)
	if err != nil {
		t.Fatal(err)
	}
	var projection JournalProjection
	if err := value.Get(&projection); err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, op := range projection.Ops {
		if op.Event != nil && op.Event.Type == journal.EventRunFinished {
			terminals++
			if op.Event.Status != "aborted" {
				t.Fatalf("cancellation terminal = %+v", op.Event)
			}
		}
	}
	want := 0
	if settled {
		want = 1
	}
	if terminals != want {
		t.Fatalf("terminal journal events = %d, want %d (settled=%t)", terminals, want, settled)
	}
}
