package engine

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestStaticBranchBodiesRealHistoryReplay(t *testing.T) {
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
	for _, mode := range []string{"task-chain", "gate", "terminal-gate", "timeout", "cancel", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			gateBody := mode == "gate" || mode == "terminal-gate"
			in := branchBodyInput("replay-"+mode, gateBody, !gateBody)
			if mode == "terminal-gate" {
				in.Run.Spec.Gates[0].Branches["pass"] = ""
			}
			if mode == "timeout" {
				in.Run.Spec.Parallels[0].BranchTimeoutSeconds = 1
			}
			in.Cancel = mode == "cancel"
			writer, _ := newLiveWriter(t)
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Auto: &fixtureAuto{localOK: true}, Det: &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				if strings.HasSuffix(env.TaskID, ":alpha") && mode == "cancel" {
					ticker := time.NewTicker(100 * time.Millisecond)
					defer ticker.Stop()
					for {
						activity.RecordHeartbeat(ctx)
						select {
						case <-ctx.Done():
							return apiv1.ResultEnvelope{}, ctx.Err()
						case <-ticker.C:
						}
					}
				}
				if strings.HasSuffix(env.TaskID, ":alpha") && mode == "timeout" {
					timer := time.NewTimer(2 * time.Second)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return apiv1.ResultEnvelope{}, ctx.Err()
					case <-timer.C:
					}
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "body"}}, nil
			}}}
			queue := "static-body-" + mode
			w := temporalworker.New(c, queue, temporalworker.Options{})
			if mode == "legacy" {
				w.RegisterWorkflowWithOptions(func(workflow.Context, branchWalkTestInput) (branchWalkTestResult, error) {
					return branchWalkTestResult{}, fmt.Errorf("engine: static parallel branch execution is refused")
				}, workflow.RegisterOptions{Name: "staticBranchBodies"})
			} else {
				w.RegisterWorkflow(staticBranchBodies)
			}
			w.RegisterActivity(acts)
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, "staticBranchBodies", in)
			if err != nil {
				t.Fatal(err)
			}
			var out branchWalkTestResult
			err = run.Get(ctx, &out)
			if (err != nil) != (mode == "legacy") {
				t.Fatalf("workflow error = %v", err)
			}
			if mode != "legacy" {
				want := journal.BranchSucceeded
				if mode == "timeout" {
					want = journal.BranchTimedOut
				}
				if mode == "cancel" {
					want = journal.BranchCancelled
				}
				if mode == "terminal-gate" {
					want = journal.BranchFailed
				}
				if out.Branches[0].Status != want || out.Branches[1].Status != journal.BranchSucceeded {
					t.Fatalf("outcomes = %+v", out.Branches)
				}
			}
			history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
			for i, event := range history.Events {
				if event.GetWorkflowTaskStartedEventAttributes() == nil {
					continue
				}
				replayer := temporalworker.NewWorkflowReplayer()
				replayer.RegisterWorkflow(staticBranchBodies)
				if err := replayer.ReplayWorkflowHistory(nil, &historypb.History{Events: history.Events[:i+1]}); err != nil {
					t.Fatalf("replay prefix %d: %v", i, err)
				}
			}
			replayer := temporalworker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(staticBranchBodies)
			if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
				t.Fatal(err)
			}
		})
	}
}
