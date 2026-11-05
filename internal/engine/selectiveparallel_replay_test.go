package engine

import (
	"context"
	"io"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/temporaltest"
)

func TestStaticSchedulerSelectiveParallelRealHistoryReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	in, acts, _ := selectiveParallelFixture(t, `{
		"schemaVersion":1,
		"branches":[{"name":"gamma","reason":"replay-selected"}]
	}`)
	const queue = "static-scheduler-selective-parallel"
	worker := temporalworker.New(server.Client(), queue, temporalworker.Options{})
	worker.RegisterWorkflow(staticScheduler)
	worker.RegisterActivity(acts)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)

	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: queue, TaskQueue: queue,
	}, staticScheduler, in)
	if err != nil {
		t.Fatal(err)
	}
	var out staticSchedulerResult
	if err := run.Get(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error != "" || out.Result.Status != StatusCompleted {
		t.Fatalf("result=%+v error=%s", out.Result, out.Error)
	}
	history := hitlFetchHistory(t, ctx, server.Client(), run.GetID(), run.GetRunID())
	for i, event := range history.Events {
		if event.GetWorkflowTaskStartedEventAttributes() == nil {
			continue
		}
		replayer := temporalworker.NewWorkflowReplayer()
		replayer.RegisterWorkflow(staticScheduler)
		if err := replayer.ReplayWorkflowHistory(nil, &historypb.History{
			Events: history.Events[:i+1],
		}); err != nil {
			t.Fatalf("replay prefix %d: %v", i, err)
		}
	}
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(staticScheduler)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatal(err)
	}
}
