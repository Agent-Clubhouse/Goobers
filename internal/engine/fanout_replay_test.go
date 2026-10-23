package engine

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestFanOutContractRealHistoryReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
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
	for _, mode := range []string{"skip", "all-skip", "empty-map", "binding", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			input := fanOutContractInput{Run: branchBodyInput("replay-contract-"+mode, false, false).Run, Mode: mode}
			writer, _ := newLiveWriter(t)
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "body"}}, nil
			}}}
			queue := "fanout-contract-" + mode
			w := temporalworker.New(c, queue, temporalworker.Options{})
			if mode == "legacy" {
				w.RegisterWorkflowWithOptions(func(workflow.Context, fanOutContractInput) (fanOutContractResult, error) {
					return fanOutContractResult{}, fmt.Errorf("legacy fan-out refusal")
				}, workflow.RegisterOptions{Name: "fanOutContractWorkflow"})
			} else {
				w.RegisterWorkflow(fanOutContractWorkflow)
			}
			w.RegisterActivity(acts)
			w.RegisterActivity(freezeContractSelection)
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, "fanOutContractWorkflow", input)
			if err != nil {
				t.Fatal(err)
			}
			var out fanOutContractResult
			err = run.Get(ctx, &out)
			if (err != nil) != (mode == "legacy") || out.Error != "" {
				t.Fatalf("run: %+v %v", out, err)
			}
			history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
			for i, event := range history.Events {
				if event.GetWorkflowTaskStartedEventAttributes() == nil {
					continue
				}
				r := temporalworker.NewWorkflowReplayer()
				r.RegisterWorkflow(fanOutContractWorkflow)
				if err := r.ReplayWorkflowHistory(nil, &historypb.History{Events: history.Events[:i+1]}); err != nil {
					t.Fatalf("prefix %d: %v", i, err)
				}
			}
			r := temporalworker.NewWorkflowReplayer()
			r.RegisterWorkflow(fanOutContractWorkflow)
			if err := r.ReplayWorkflowHistory(nil, history); err != nil {
				t.Fatal(err)
			}
		})
	}
}
