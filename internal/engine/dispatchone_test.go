package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/temporaltest"
)

// dispatchOneWorkflowID is the per-attempt identity decision 003 ruling 2
// gives a dispatch: <runID>/<stage>/<attempt>. It is asserted here as a
// literal rather than composed, because two properties depend on its SHAPE
// and not merely on its uniqueness: DS6 liveness keys on WorkflowID == RunID
// and must never match it, and completedRunDir refuses it as a path segment.
const dispatchOneWorkflowID = "run-dispatch-one/build/1"

// TestDispatchOne is decision 003 ruling 2's acceptance for the transport:
// one Temporal workflow, started by a CLIENT, places one stage attempt on the
// pinned dispatch queue and hands back the pod's result with the provenance
// of where it actually ran — and is invisible to every reader that keys off a
// run's gaggle memo.
//
// Both subtests share one dev server because the second one's subject is the
// closed execution the first one produces: the visibility record of a real
// DispatchOne, not a hand-built WorkflowExecutionInfo.
func TestDispatchOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})

	const workflowQueue = "dispatch-one-workflow"
	dispatchQueue := dispatcher.QueueName("web", "win-ci")
	queuedAt := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	podStartedAt := queuedAt.Add(42 * time.Second)

	store := surrenderStore(t)
	putSurrendered(t, store, "run-dispatch-one", "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{
			Status:  apiv1.ResultSuccess,
			Summary: "built in a pod for the daemon",
			Outputs: map[string]interface{}{"artifact": "bin/app.exe"},
		},
		WorkspaceDelta: "sha256:9ab31c",
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "win-ci", Pod: "goobers-run-dispatch-one-build-1", Image: "ghcr.io/example/win:v1",
		Phase: corev1.PodSucceeded, SurrenderConfirmed: true, Disposed: true,
		QueuedAt: queuedAt, PodStartedAt: podStartedAt,
	}}

	temporalClient := server.Client()
	// The workflow queue's worker has NO dispatcher wired. If DispatchOne
	// failed to route the activity onto the PINNED queue it would be served
	// here and fail "not configured" — the same negative control the engine's
	// own mode-3 routing test uses, and the reason this asserts routing rather
	// than merely asserting that some worker ran the activity.
	workflowWorker := temporalworker.New(temporalClient, workflowQueue, temporalworker.Options{})
	RegisterWith(workflowWorker, &Activities{Workspaces: testWorkspaces(t)})
	if err := workflowWorker.Start(); err != nil {
		t.Fatalf("start workflow-queue worker: %v", err)
	}
	t.Cleanup(workflowWorker.Stop)
	dispatchWorker := temporalworker.New(temporalClient, dispatchQueue, temporalworker.Options{})
	RegisterWith(dispatchWorker, &Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: store})
	if err := dispatchWorker.Start(); err != nil {
		t.Fatalf("start dispatch-queue worker: %v", err)
	}
	t.Cleanup(dispatchWorker.Stop)

	in := dispatchInput("run-dispatch-one", "build", 1)
	in.Run = &apiv1.DeterministicRun{Command: []string{"build.cmd"}, Workspace: apiv1.WorkspaceScratch}

	// Started exactly the way the daemon's runner will start it (ruling 2):
	// a client, a deterministic per-attempt WorkflowID, REJECT_DUPLICATE, and
	// NO Memo argument at all.
	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    dispatchOneWorkflowID,
		TaskQueue:             workflowQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, DispatchOne, in)
	if err != nil {
		t.Fatalf("start DispatchOne: %v", err)
	}
	var result DispatchStageResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatalf("DispatchOne result: %v", err)
	}

	t.Run("routes the activity onto the pinned dispatch queue", func(t *testing.T) {
		if calls := fake.calls.Load(); calls != 1 {
			t.Fatalf("dispatcher calls = %d, want 1 — the dispatch queue's worker must have served the stage", calls)
		}
		attempts, eligible := fake.recorded()
		if attempts[0].RunID != "run-dispatch-one" || attempts[0].Stage != "build" || attempts[0].Number != 1 {
			t.Fatalf("attempt = %+v, want the attempt identity built from the activity input", attempts[0])
		}
		if len(eligible[0]) != 1 || eligible[0][0].Name != "win-ci" {
			t.Fatalf("eligible = %+v, want the pinned eligible set passed through untouched", eligible[0])
		}
	})

	t.Run("returns the surrendered result", func(t *testing.T) {
		if result.Status != apiv1.ResultSuccess || result.Summary != "built in a pod for the daemon" {
			t.Fatalf("result envelope = %+v, want the pod's surrendered envelope", result.ResultEnvelope)
		}
		if got := result.Outputs["artifact"]; got != "bin/app.exe" {
			t.Fatalf("outputs = %v, want the surrendered outputs", result.Outputs)
		}
		if result.WorkspaceDelta != "sha256:9ab31c" {
			t.Fatalf("workspace delta = %q, want the surrendered digest carried back to the driver", result.WorkspaceDelta)
		}
	})

	// §11 acceptance 6: the driver journals runner.placement from what the
	// SUBSTRATE reports, so every field has to survive the activity boundary
	// and the history round-trip.
	t.Run("returns the placement provenance the driver journals", func(t *testing.T) {
		if result.Placement == nil {
			t.Fatal("placement provenance is nil; a pod-executed attempt must report where it ran")
		}
		want := StagePlacement{
			Runner: "win-ci", Pod: "goobers-run-dispatch-one-build-1", Image: "ghcr.io/example/win:v1",
			QueuedAt: queuedAt, PodStartedAt: podStartedAt,
		}
		got := *result.Placement
		if got.Runner != want.Runner || got.Pod != want.Pod || got.Image != want.Image ||
			!got.QueuedAt.Equal(want.QueuedAt) || !got.PodStartedAt.Equal(want.PodStartedAt) {
			t.Fatalf("placement = %+v, want %+v (lifted verbatim from dispatcher.Report)", got, want)
		}
	})

	t.Run("carries no run gaggle memo, so the projection reconciler ignores it", func(t *testing.T) {
		info := awaitClosedExecution(ctx, t, temporalClient, dispatchOneWorkflowID)
		if memo := info.GetMemo().GetFields()[RunGaggleMemoKey]; memo != nil {
			t.Fatalf("DispatchOne visibility record carries %s; the reconciler and DS6 liveness both key off it", RunGaggleMemoKey)
		}

		runsDir := t.TempDir()
		var observed []string
		reconciler, err := NewCompletedRunReconciler(temporalClient, "default", map[string]string{"web": runsDir},
			func(_ context.Context, runID string, _ uint64) error {
				observed = append(observed, runID)
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		// The reconciler runs against the LIVE namespace this DispatchOne just
		// closed in, so it walks the real visibility page the daemon's
		// reconciler would walk.
		projected, err := reconciler.Reconcile(ctx)
		if err != nil {
			t.Fatalf("Reconcile over a namespace holding one DispatchOne: %v", err)
		}
		if projected != 0 || len(observed) != 0 {
			t.Fatalf("projected = %d, observed = %v, want the dispatch workflow ignored entirely", projected, observed)
		}
		if entries, err := os.ReadDir(runsDir); err != nil || len(entries) != 0 {
			t.Fatalf("runs dir entries = %v (err %v), want nothing written for a dispatch workflow", entries, err)
		}

		// Non-vacuity: the SAME visibility record, with the gaggle memo
		// stamped on, is not ignored — the reconciler reaches
		// completedRunDir and refuses the id as a path segment. So the memo's
		// absence is doing the work, not the id's shape or an empty page.
		gaggle, err := converter.GetDefaultDataConverter().ToPayload("web")
		if err != nil {
			t.Fatal(err)
		}
		withMemo := &completedRunFake{executions: []*workflowpb.WorkflowExecutionInfo{{
			Execution: &commonpb.WorkflowExecution{WorkflowId: dispatchOneWorkflowID},
			Memo:      &commonpb.Memo{Fields: map[string]*commonpb.Payload{RunGaggleMemoKey: gaggle}},
		}}}
		control, err := NewCompletedRunReconciler(withMemo, "default", map[string]string{"web": t.TempDir()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := control.Reconcile(ctx); !errors.Is(err, ErrUnprojectable) {
			t.Fatalf("control Reconcile error = %v, want %v — with the memo the reconciler DOES attempt the dispatch workflow", err, ErrUnprojectable)
		}
	})
}

// awaitClosedExecution returns the visibility record for workflowID once it is
// closed and indexed. The dev server's visibility store is eventually
// consistent with the history store, so this polls rather than reading once.
func awaitClosedExecution(ctx context.Context, t *testing.T, c client.Client, workflowID string) *workflowpb.WorkflowExecutionInfo {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Namespace: "default",
			PageSize:  completedRunPageSize,
			Query:     "ExecutionStatus != 'Running'",
		})
		if err != nil {
			t.Fatalf("list closed workflows: %v", err)
		}
		for _, info := range response.GetExecutions() {
			if info.GetExecution().GetWorkflowId() == workflowID {
				return info
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow %q never appeared as closed in visibility", workflowID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestRecordedDispatchStageHistoryReplays is the wire-contract guard decision
// 003 plan step 3 requires: "engine.DispatchStageInput (JSON unchanged)".
//
// testdata/dispatchstage_history.json is a REAL history, recorded off a
// two-stage pod-dispatched run before DispatchStageInput/DispatchStageResult
// were exported and before placement provenance existed. It is checked in
// precisely so a future change to those types is measured against bytes
// Temporal already wrote, rather than against bytes the test itself just
// produced — a test that records and replays in one process cannot fail on a
// tag rename, because both sides rename together.
//
// Two things are asserted, and they fail for different reasons:
//   - the history replays, so the workflow's COMMAND sequence is unchanged;
//   - the recorded activity payloads still decode into the exported types
//     field for field, so no JSON tag moved. Replay alone would not catch
//     that: encoding/json ignores keys it does not know, so a renamed tag
//     silently produces a zero-valued field instead of an error.
func TestRecordedDispatchStageHistoryReplays(t *testing.T) {
	data, err := os.ReadFile("testdata/dispatchstage_history.json")
	if err != nil {
		t.Fatalf("read recorded history: %v", err)
	}
	history := &historypb.History{}
	if err := protojson.Unmarshal(data, history); err != nil {
		t.Fatalf("decode recorded history: %v", err)
	}

	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay the recorded mode-3 history: %v", err)
	}

	inputs, results := recordedDispatchPayloads(t, history)
	if len(inputs) != 2 || len(results) != 2 {
		t.Fatalf("recorded DispatchStage activities = %d inputs / %d results, want 2 each", len(inputs), len(results))
	}

	build := inputs[0]
	if build.Envelope.TaskID != "run-recorded-dispatch:build" || build.Envelope.Gaggle != "web" {
		t.Fatalf("envelope = %+v, want the recorded envelope (json tag \"envelope\")", build.Envelope)
	}
	wantPlacement := PinnedPlacement{
		Stage: "build", Queue: "goobers-dispatch.web.win-ci",
		Eligible: []dispatcher.RunnerSpec{{
			Name: "win-ci", OS: "windows", HostKind: "image", Host: "ghcr.io/example/win:v1",
			Memory: "16Gi", Restrictions: []string{"tmp:ephemeral"},
		}},
		CPU: "2", Memory: "4Gi", Disk: "10Gi", Restrictions: []string{"tmp:ephemeral"},
	}
	if got := build.Placement; !reflect.DeepEqual(got, wantPlacement) {
		t.Fatalf("placement = %+v, want %+v (json tag \"placement\" and every field tag beneath it)", got, wantPlacement)
	}
	if build.Run == nil || len(build.Run.Command) != 2 || build.Run.Command[0] != "build.cmd" ||
		build.Run.Env["BUILD_MODE"] != "release" || build.Run.Workspace != apiv1.WorkspaceRepo {
		t.Fatalf("run = %+v, want the recorded DeterministicRun (json tag \"run\")", build.Run)
	}

	publish := inputs[1]
	if !publish.Placement.LedgerTouching {
		t.Fatal("placement.ledgerTouching did not decode from the recorded history")
	}
	if publish.Run == nil || publish.Run.Script != "publish.ps1" {
		t.Fatalf("run = %+v, want the recorded script stage", publish.Run)
	}
	if publish.Workspace != apiv1.WorkspaceRepo {
		t.Fatalf("workspace = %q, want the recorded task-level workspace (json tag \"workspace\")", publish.Workspace)
	}
	// Carried from the first stage's surrendered result into the second
	// stage's input: the #3763 hand-off, recorded across two activities.
	if publish.WorkspaceDelta != "sha256:5f2b1c" {
		t.Fatalf("workspaceDelta = %q, want the digest the first stage surrendered (json tag \"workspaceDelta\")", publish.WorkspaceDelta)
	}

	built := results[0]
	if built.Status != apiv1.ResultSuccess || built.Summary != "built remotely" || built.Outputs["artifact"] != "bin/app.exe" {
		t.Fatalf("result = %+v, want the recorded envelope decoded through the embedded ResultEnvelope", built.ResultEnvelope)
	}
	if len(built.Mutations) != 1 || built.Mutations[0] != (mutationFact{Provider: "github", Kind: "pr", ID: "7", Operation: "open"}) {
		t.Fatalf("mutations = %+v, want the recorded mutation fact (json tag \"mutations\")", built.Mutations)
	}
	if len(built.MutationIssues) != 1 || built.WorkspaceDelta != "sha256:5f2b1c" {
		t.Fatalf("mutationIssues = %v / workspaceDelta = %q, want the recorded values", built.MutationIssues, built.WorkspaceDelta)
	}
	// Provenance is ADDITIVE: a history written before StagePlacement existed
	// decodes with none, rather than failing or inventing one.
	if built.Placement != nil {
		t.Fatalf("placement = %+v, want nil for a history recorded before provenance existed", built.Placement)
	}
}

// recordedDispatchPayloads pulls the DispatchStage activity input and result
// payloads out of a recorded history, in event order, decoding each through
// the SAME data converter Temporal recorded it with.
func recordedDispatchPayloads(t *testing.T, history *historypb.History) ([]DispatchStageInput, []DispatchStageResult) {
	t.Helper()
	dc := converter.GetDefaultDataConverter()
	var (
		inputs    []DispatchStageInput
		results   []DispatchStageResult
		scheduled = map[int64]bool{}
	)
	for _, event := range history.GetEvents() {
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			attrs := event.GetActivityTaskScheduledEventAttributes()
			if attrs.GetActivityType().GetName() != ActDispatchStage {
				continue
			}
			scheduled[event.GetEventId()] = true
			var in DispatchStageInput
			if err := dc.FromPayloads(attrs.GetInput(), &in); err != nil {
				t.Fatalf("decode recorded DispatchStage input at event %d: %v", event.GetEventId(), err)
			}
			inputs = append(inputs, in)
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			attrs := event.GetActivityTaskCompletedEventAttributes()
			if !scheduled[attrs.GetScheduledEventId()] {
				continue
			}
			var result DispatchStageResult
			if err := dc.FromPayloads(attrs.GetResult(), &result); err != nil {
				t.Fatalf("decode recorded DispatchStage result at event %d: %v", event.GetEventId(), err)
			}
			results = append(results, result)
		}
	}
	return inputs, results
}

// The activity name the recorded history schedules is the one the workflow
// still dispatches. Cheap, but it is the one string a rename would break in
// both the fixture and the code at once if it were only asserted through
// replay.
func TestRecordedHistoryNamesTheDispatchActivity(t *testing.T) {
	data, err := os.ReadFile("testdata/dispatchstage_history.json")
	if err != nil {
		t.Fatalf("read recorded history: %v", err)
	}
	if !strings.Contains(string(data), ActDispatchStage) {
		t.Fatalf("recorded history does not name activity %q", ActDispatchStage)
	}
}
