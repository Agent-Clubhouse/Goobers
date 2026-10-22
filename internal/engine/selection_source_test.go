package engine

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestSelectionFrozenBeforeFanout(t *testing.T) {
	store, input := selectionActivityFixture(t)
	activities := &Activities{FanOutBlobs: store}
	got, err := activities.FreezeParallelSelection(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParallelName != input.ParallelName ||
		got.Selection.SourceDigest != input.Source.Digest ||
		got.Selection.Integrity != apiv1.IntegrityUnapproved {
		t.Fatalf("freeze result = %+v", got)
	}
	if !reflect.DeepEqual(got.Selection.Selected, []string{"optional-a", "optional-b"}) ||
		len(got.Selection.Reasons) != 1 || got.Selection.Reasons["optional-b"] != "needed" {
		t.Fatalf("normalized selection = %+v", got.Selection)
	}

	again, err := activities.FreezeParallelSelection(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("identical retry changed selection:\nfirst %+v\nagain %+v", got, again)
	}
}

func TestFreezeParallelSelectionFailsClosed(t *testing.T) {
	store, input := selectionActivityFixture(t)
	tests := map[string]func(*Activities, *FreezeParallelSelectionInput){
		"no store": func(a *Activities, _ *FreezeParallelSelectionInput) {
			a.FanOutBlobs = nil
		},
		"no run": func(_ *Activities, in *FreezeParallelSelectionInput) {
			in.RunID = ""
		},
		"parallel mismatch": func(_ *Activities, in *FreezeParallelSelectionInput) {
			in.ParallelName = "other"
		},
		"source digest": func(_ *Activities, in *FreezeParallelSelectionInput) {
			in.Source.Digest = journal.Digest([]byte("forged"))
		},
		"source size": func(_ *Activities, in *FreezeParallelSelectionInput) {
			in.Source.Size++
		},
		"media type": func(_ *Activities, in *FreezeParallelSelectionInput) {
			in.Source.MediaType = "text/plain"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			activities := &Activities{FanOutBlobs: store}
			gotInput := input
			mutate(activities, &gotInput)
			if _, err := activities.FreezeParallelSelection(context.Background(), gotInput); err == nil {
				t.Fatal("FreezeParallelSelection accepted invalid input")
			}
		})
	}
}

func TestFreezeParallelSelectionHistoryReplaysAtEveryWorkflowTaskStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})
	store, input := selectionActivityFixture(t)
	const queue = "parallel-selection-freeze-replay"
	worker := temporalworker.New(server.Client(), queue, temporalworker.Options{})
	worker.RegisterWorkflow(freezeParallelSelectionProbe)
	worker.RegisterActivity(&Activities{FanOutBlobs: store})
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)

	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "parallel-selection-freeze-replay", TaskQueue: queue,
	}, freezeParallelSelectionProbe, input)
	if err != nil {
		t.Fatal(err)
	}
	var result FreezeParallelSelectionResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	history := readSelectionFreezeHistory(t, ctx, server.Client(), run)
	replaySelectionFreezeHistory(t, history, "full")
	prefixes := 0
	for i, event := range history.Events {
		if event.EventType != enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			continue
		}
		prefixes++
		replaySelectionFreezeHistory(t, &historypb.History{
			Events: append([]*historypb.HistoryEvent(nil), history.Events[:i+1]...),
		}, "prefix")
	}
	if prefixes < 2 {
		t.Fatalf("history has %d workflow-task-start prefixes, want at least activity schedule and completion", prefixes)
	}
}

func freezeParallelSelectionProbe(
	ctx workflow.Context,
	input FreezeParallelSelectionInput,
) (FreezeParallelSelectionResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Millisecond, MaximumAttempts: 2,
		},
	})
	var result FreezeParallelSelectionResult
	err := workflow.ExecuteActivity(ctx, ActFreezeParallelSelection, input).Get(ctx, &result)
	return result, err
}

func readSelectionFreezeHistory(
	t *testing.T,
	ctx context.Context,
	c client.Client,
	run client.WorkflowRun,
) *historypb.History {
	t.Helper()
	iter := c.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false,
		enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	history := &historypb.History{}
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		history.Events = append(history.Events, event)
	}
	return history
}

func replaySelectionFreezeHistory(t *testing.T, history *historypb.History, label string) {
	t.Helper()
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(freezeParallelSelectionProbe)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay %s history: %v", label, err)
	}
}

func selectionActivityFixture(t *testing.T) (*blobstore.Dir, FreezeParallelSelectionInput) {
	t.Helper()
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sourceData := []byte(`{"schemaVersion":1,"branches":[{"name":"optional-b","reason":"needed"},{"name":"required"},{"name":"optional-a"}]}`)
	source := journal.FanOutSourceRef{
		Task: "plan", Slot: "selection", Visit: 11, Attempt: 2,
		Digest: journal.Digest(sourceData), Integrity: apiv1.IntegrityUnapproved,
		Path: "artifacts/selection.json", Size: int64(len(sourceData)), MediaType: fanout.SourceMediaType,
	}
	if err := store.Put(context.Background(), source.Digest, sourceData); err != nil {
		t.Fatal(err)
	}
	definition := apiv1.Parallel{
		Name: "review", FailurePolicy: apiv1.BranchContinueOnError, Join: "collate",
		Selection: &apiv1.ParallelSelection{Source: apiv1.FanOutSource{Task: "plan", Slot: "selection"}},
		Branches: []apiv1.Branch{
			{Name: "required", Start: "required"},
			{Name: "optional-a", Start: "optional-a", Optional: true},
			{Name: "optional-b", Start: "optional-b", Optional: true},
		},
	}
	return store, FreezeParallelSelectionInput{
		RunID: "selection-freeze-run", ParallelName: definition.Name,
		Definition: definition, Source: source,
	}
}

func TestSelectionFreezeReasonBoundIsBytes(t *testing.T) {
	store, input := selectionActivityFixture(t)
	reason := strings.Repeat("é", fanout.MaxSelectionReasonBytes/2+1)
	data := []byte(`{"schemaVersion":1,"branches":[{"name":"optional-a","reason":"` + reason + `"}]}`)
	input.Source.Digest, input.Source.Size = journal.Digest(data), int64(len(data))
	if err := store.Put(context.Background(), input.Source.Digest, data); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Activities{FanOutBlobs: store}).FreezeParallelSelection(context.Background(), input); err == nil {
		t.Fatal("selection reason exceeding the byte bound was accepted")
	}
}
