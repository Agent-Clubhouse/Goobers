package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestStaticSchedulerRealHistoryReplay(t *testing.T) {
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
	for _, mode := range []string{"bounded", "budget", "timeout", "cancel", "gate-join", "review-join", "branch-inputs", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			in := schedulerInput("replay-scheduler-"+mode, 2)
			if mode == "gate-join" {
				in = gateJoinInput("replay-scheduler-"+mode, apiv1.EvaluatorAutomated)
			}
			if mode == "review-join" {
				in = gateJoinInput("replay-scheduler-"+mode, apiv1.EvaluatorAgentic)
			}
			if mode == "branch-inputs" {
				in.Run.Spec.Tasks[1].ExpectedOutputs = []string{"value"}
				in.Run.Spec.Tasks[3].InputsFrom = map[string]string{"implicit": "parallel.alpha.value", "explicit": "parallel.alpha.alpha.value"}
			}
			if mode == "budget" {
				in.Limit = 3
			}
			if mode == "timeout" {
				in.Run.Spec.Parallels[0].MaxConcurrentBranches = 1
				in.Run.Spec.Parallels[0].BranchTimeoutSeconds = 1
				in.Run.Spec.Tasks[1].Next = "after"
				after := detTask("after", "")
				after.Next = wf.TargetJoin
				in.Run.Spec.Tasks = append(in.Run.Spec.Tasks, after)
			}
			writer, dir := newLiveWriter(t)
			started := make(chan struct{}, 3)
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				stage := stageOfTaskID(env.TaskID)
				if mode == "branch-inputs" && stage == "join" && (env.Inputs["implicit"] != "frozen-root" || env.Inputs["explicit"] != "frozen-root") {
					return apiv1.ResultEnvelope{}, fmt.Errorf("branch-qualified join lost its settled products")
				}
				if mode == "cancel" && stage != "seed" && stage != "join" {
					started <- struct{}{}
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
				if mode == "timeout" && stage == "alpha" {
					timer := time.NewTimer(2 * time.Second)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return apiv1.ResultEnvelope{}, ctx.Err()
					case <-timer.C:
					}
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "frozen-root", runner.BranchCompletenessInput: "FORGED"}}, nil
			}}}
			acts.Auto = automatedFunc(func(_ context.Context, _ apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
				return "pass", validateReplayJoinCompleteness(env)
			})
			acts.Goober = &fakeInvoker{review: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
				return apiv1.Verdict{Decision: apiv1.VerdictPass}, validateReplayJoinCompleteness(env)
			}}

			queue := "static-scheduler-" + mode
			w := temporalworker.New(c, queue, temporalworker.Options{})
			if mode == "legacy" {
				w.RegisterWorkflowWithOptions(func(workflow.Context, staticSchedulerInput) (staticSchedulerResult, error) {
					return staticSchedulerResult{}, fmt.Errorf("engine: static parallel execution is refused")
				}, workflow.RegisterOptions{Name: "staticScheduler"})
			} else {
				w.RegisterWorkflow(staticScheduler)
			}
			w.RegisterActivity(acts)
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: queue, TaskQueue: queue}, "staticScheduler", in)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				for range 2 {
					select {
					case <-started:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				if err := c.CancelWorkflow(ctx, run.GetID(), run.GetRunID()); err != nil {
					t.Fatal(err)
				}
			}
			var out staticSchedulerResult
			err = run.Get(ctx, &out)
			if (err != nil) != (mode == "legacy") {
				t.Fatalf("workflow error=%v", err)
			}
			if (mode == "bounded" || mode == "gate-join" || mode == "review-join" || mode == "branch-inputs") && (out.Result.Status != StatusCompleted || out.Error != "" || out.Result.Steps != 6) {
				t.Fatalf("result=%+v error=%s", out.Result, out.Error)
			}
			if mode == "budget" && (out.Error == "" || out.Result.Steps != 4) {
				t.Fatalf("budget result=%+v error=%s", out.Result, out.Error)
			}
			if mode == "timeout" {
				assertSchedulerCompleteness(t, liveEvents(t, dir, in.Run.RunID), []journal.BranchStatus{journal.BranchTimedOut, journal.BranchSucceeded, journal.BranchSucceeded}, "join")
			}
			if mode == "cancel" {
				assertSchedulerCompleteness(t, liveEvents(t, dir, in.Run.RunID), []journal.BranchStatus{journal.BranchCancelled, journal.BranchCancelled, journal.BranchCancelled}, "")
			}
			history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
			for i, event := range history.Events {
				if event.GetWorkflowTaskStartedEventAttributes() == nil {
					continue
				}
				replayer := temporalworker.NewWorkflowReplayer()
				replayer.RegisterWorkflow(staticScheduler)
				if err := replayer.ReplayWorkflowHistory(nil, &historypb.History{Events: history.Events[:i+1]}); err != nil {
					t.Fatalf("replay prefix %d: %v", i, err)
				}
			}
			replayer := temporalworker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(staticScheduler)
			if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func validateReplayJoinCompleteness(env apiv1.InvocationEnvelope) error {
	data, err := json.Marshal(env.Inputs[runner.BranchCompletenessInput])
	if err != nil {
		return err
	}
	var got []journal.BranchOutcome
	if err := json.Unmarshal(data, &got); err != nil {
		return err
	}
	if len(got) != 3 {
		return fmt.Errorf("missing host join completeness: %s", data)
	}
	for i, outcome := range got {
		if outcome.Branch != i+1 || outcome.Status != journal.BranchSucceeded {
			return fmt.Errorf("invalid host join completeness: %s", data)
		}
	}
	return nil
}
