package engine

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/temporaltest"
)

func TestJournalOwnerRealHistoriesReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	c := server.Client()
	for _, mode := range []string{"success", "ack-loss", "before-write", "bad-ack", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			writer, dir := newLiveWriter(t)
			emitter := &ownerEmitter{writer: writer, mode: mode}
			queue := "journal-owner-" + mode
			w := temporalworker.New(c, queue, temporalworker.Options{})
			w.RegisterWorkflow(journalOwnerWorkflow)
			w.RegisterActivityWithOptions(emitter.Emit, activity.RegisterOptions{Name: ActEmitJournal})
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, journalOwnerWorkflow, mode)
			if err != nil {
				t.Fatal(err)
			}
			var projection JournalProjection
			if err := run.Get(ctx, &projection); err != nil {
				t.Fatal(err)
			}
			assertJournalOwnerOutcome(t, projection, liveEvents(t, dir, projection.Identity.RunID), mode)
			history := &historypb.History{}
			iter := c.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
			for iter.HasNext() {
				event, err := iter.Next()
				if err != nil {
					t.Fatal(err)
				}
				history.Events = append(history.Events, event)
			}
			// Every workflow task start prefix is a valid worker-loss/eviction
			// boundary; each must recover the same queue and branch-local state.
			for i, event := range history.Events {
				if event.GetWorkflowTaskStartedEventAttributes() == nil {
					continue
				}
				replayer := temporalworker.NewWorkflowReplayer()
				replayer.RegisterWorkflow(journalOwnerWorkflow)
				if err := replayer.ReplayWorkflowHistory(nil, &historypb.History{Events: history.Events[:i+1]}); err != nil {
					t.Fatalf("replay prefix %d: %v", i, err)
				}
			}
			replayer := temporalworker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(journalOwnerWorkflow)
			if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("unmarked-legacy", func(t *testing.T) {
		queue := "journal-owner-legacy"
		w := temporalworker.New(c, queue, temporalworker.Options{})
		w.RegisterWorkflowWithOptions(func(workflow.Context, string) (JournalProjection, error) {
			return JournalProjection{}, errors.New("legacy branch refusal")
		}, workflow.RegisterOptions{Name: "journalOwnerWorkflow"})
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		defer w.Stop()
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, "journalOwnerWorkflow", "legacy")
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Get(ctx, nil); err == nil {
			t.Fatal("legacy branch unexpectedly executed")
		}
		history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
		for _, event := range history.Events {
			if event.GetEventType() == enumspb.EVENT_TYPE_MARKER_RECORDED || event.GetEventType() == enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED {
				t.Fatal("legacy refusal contains protocol commands")
			}
		}
		replayer := temporalworker.NewWorkflowReplayer()
		replayer.RegisterWorkflow(journalOwnerWorkflow)
		if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
			t.Fatal(err)
		}
	})

}
