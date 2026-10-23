package engine

import (
	"context"
	"errors"
	"io"
	"reflect"
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
)

func TestMapRosterFrozen(t *testing.T) {
	store, input, sourceData := mapRosterActivityFixture(t)
	activities := &Activities{FanOutBlobs: store}
	got, err := activities.FreezeMapRoster(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got.MapName != input.MapName || got.Source != input.Source ||
		got.RosterDigest == "" || got.RosterSize <= 0 {
		t.Fatalf("freeze result = %+v", got)
	}
	wantItems := []MapRosterItem{
		{Identity: "a", Digest: mustMapItemDigest(t, "a", `{"shard":"a"}`)},
		{Identity: "b", Digest: mustMapItemDigest(t, "b", `{"shard":"b"}`)},
	}
	if !reflect.DeepEqual(got.Items, wantItems) {
		t.Fatalf("items = %+v, want %+v", got.Items, wantItems)
	}
	rosterData, err := store.Get(context.Background(), got.RosterDigest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := fanout.DecodeRosterBlob(rosterData, got.RosterDigest, input.Definition)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Roster.Source.Digest != journal.Digest(sourceData) ||
		len(decoded.Roster.Items) != len(wantItems) {
		t.Fatalf("stored roster = %+v", decoded.Roster)
	}

	again, err := activities.FreezeMapRoster(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("identical retry changed result:\nfirst %+v\nagain %+v", got, again)
	}
}

func TestFreezeMapRosterFailsClosed(t *testing.T) {
	store, input, _ := mapRosterActivityFixture(t)
	tests := map[string]func(*Activities, *FreezeMapRosterInput){
		"no store":      func(a *Activities, _ *FreezeMapRosterInput) { a.FanOutBlobs = nil },
		"no run":        func(_ *Activities, in *FreezeMapRosterInput) { in.RunID = "" },
		"map mismatch":  func(_ *Activities, in *FreezeMapRosterInput) { in.MapName = "other" },
		"source digest": func(_ *Activities, in *FreezeMapRosterInput) { in.Source.Digest = journal.Digest([]byte("forged")) },
		"source size":   func(_ *Activities, in *FreezeMapRosterInput) { in.Source.Size++ },
		"media type":    func(_ *Activities, in *FreezeMapRosterInput) { in.Source.MediaType = "text/plain" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			activities := &Activities{FanOutBlobs: store}
			gotInput := input
			mutate(activities, &gotInput)
			if _, err := activities.FreezeMapRoster(context.Background(), gotInput); err == nil {
				t.Fatal("FreezeMapRoster accepted invalid input")
			}
		})
	}
}

func TestFreezeMapRosterChecksReferenceBeforeBlobRead(t *testing.T) {
	store, input, _ := mapRosterActivityFixture(t)
	tests := map[string]func(*FreezeMapRosterInput){
		"oversize": func(input *FreezeMapRosterInput) {
			input.Source.Size = fanout.MaxSourceBytes + 1
		},
		"malformed digest": func(input *FreezeMapRosterInput) {
			input.Source.Digest = "sha256:not-a-digest"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			counting := &countingBlobStore{Store: store}
			gotInput := input
			mutate(&gotInput)
			if _, err := (&Activities{FanOutBlobs: counting}).FreezeMapRoster(
				context.Background(), gotInput,
			); err == nil {
				t.Fatal("FreezeMapRoster accepted invalid source reference")
			}
			if counting.gets != 0 {
				t.Fatalf("blob store read %d time(s) before rejecting source reference", counting.gets)
			}
		})
	}
}

func TestFreezeMapRosterSeparatesCallerAndSourceErrors(t *testing.T) {
	store, input, _ := mapRosterActivityFixture(t)
	tests := []struct {
		name     string
		mutate   func(*FreezeMapRosterInput)
		wantType string
	}{
		{
			name: "caller", wantType: "fanout_caller_invalid",
			mutate: func(input *FreezeMapRosterInput) { input.RunID = "" },
		},
		{
			name: "source", wantType: fanout.CodeSourceInvalid,
			mutate: func(input *FreezeMapRosterInput) { input.Source.Size++ },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotInput := input
			test.mutate(&gotInput)
			_, err := (&Activities{FanOutBlobs: store}).FreezeMapRoster(context.Background(), gotInput)
			var application *temporal.ApplicationError
			if !errors.As(err, &application) || application.Type() != test.wantType {
				t.Fatalf("error = %T %v, want application type %q", err, err, test.wantType)
			}
		})
	}
}

func TestFreezeMapRosterHistoryReplaysAtEveryWorkflowTaskStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, stop, err := startReplayClient(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})
	store, input, _ := mapRosterActivityFixture(t)
	const queue = "map-roster-freeze-replay"
	worker := temporalworker.New(c, queue, temporalworker.Options{})
	worker.RegisterWorkflow(freezeMapRosterProbe)
	worker.RegisterActivity(&Activities{FanOutBlobs: store})
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)

	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "map-roster-freeze-replay", TaskQueue: queue,
	}, freezeMapRosterProbe, input)
	if err != nil {
		t.Fatal(err)
	}
	var result FreezeMapRosterResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	history := readMapFreezeHistory(t, ctx, c, run)
	replayMapFreezeHistory(t, history, "full")
	prefixes := 0
	for i, event := range history.Events {
		if event.EventType != enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			continue
		}
		prefixes++
		replayMapFreezeHistory(t, &historypb.History{
			Events: append([]*historypb.HistoryEvent(nil), history.Events[:i+1]...),
		}, "prefix")
	}
	if prefixes < 2 {
		t.Fatalf("history has %d workflow-task-start prefixes, want at least activity schedule and completion", prefixes)
	}
}

func freezeMapRosterProbe(ctx workflow.Context, input FreezeMapRosterInput) (FreezeMapRosterResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Millisecond, MaximumAttempts: 2,
		},
	})
	var result FreezeMapRosterResult
	err := workflow.ExecuteActivity(ctx, ActFreezeMapRoster, input).Get(ctx, &result)
	return result, err
}

func readMapFreezeHistory(
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

func replayMapFreezeHistory(t *testing.T, history *historypb.History, label string) {
	t.Helper()
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(freezeMapRosterProbe)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay %s history: %v", label, err)
	}
}

func mapRosterActivityFixture(t *testing.T) (*blobstore.Dir, FreezeMapRosterInput, []byte) {
	t.Helper()
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sourceData := []byte(`{"schemaVersion":1,"items":[{"identity":"b","item":{"shard":"b"}},{"identity":"a","item":{"shard":"a"}}]}`)
	source := journal.FanOutSourceRef{
		Task: "plan", Slot: "roster", Visit: 9, Attempt: 1,
		Digest: journal.Digest(sourceData), Integrity: apiv1.IntegrityMaintainer,
		Path: "artifacts/roster.json", Size: int64(len(sourceData)), MediaType: fanout.SourceMediaType,
	}
	if err := store.Put(context.Background(), source.Digest, sourceData); err != nil {
		t.Fatal(err)
	}
	definition := apiv1.Map{
		Name: "shards", Source: apiv1.FanOutSource{Task: "plan", Slot: "roster"}, Task: "work",
		FailurePolicy: apiv1.BranchContinueOnError, Join: "collate",
		ItemTimeoutSeconds: 30, MaxConcurrent: 2,
	}
	return store, FreezeMapRosterInput{
		RunID: "map-freeze-run", MapName: definition.Name,
		Definition: definition, Source: source,
	}, sourceData
}

func mustMapItemDigest(t *testing.T, identity, item string) string {
	t.Helper()
	digest, err := fanout.ItemDigest(identity, []byte(item))
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

type countingBlobStore struct {
	blobstore.Store
	gets int
}

func (s *countingBlobStore) Get(ctx context.Context, digest string) ([]byte, error) {
	s.gets++
	return s.Store.Get(ctx, digest)
}
