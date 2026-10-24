//go:build integration

package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type cancelOutcomeExecutor struct {
	acknowledge bool
	started     chan struct{}
	release     chan struct{}
	cancelSeen  chan struct{}
	once        sync.Once
	cancelOnce  sync.Once
}

func (e *cancelOutcomeExecutor) Run(
	ctx context.Context,
	_ apiv1.InvocationEnvelope,
	_ apiv1.DeterministicRun,
) (apiv1.ResultEnvelope, error) {
	e.once.Do(func() { close(e.started) })
	if e.acknowledge {
		select {
		case <-ctx.Done():
			e.cancelOnce.Do(func() { close(e.cancelSeen) })
			return apiv1.ResultEnvelope{}, ctx.Err()
		case <-time.After(25 * time.Second):
			return apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess,
			}, nil
		}
	}
	<-e.release
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func cancelOutcomeRunInput(runID string) RunInput {
	preview := true
	return RunInput{
		RunID: runID, Gaggle: "web",
		WorkflowName: "cancel-outcome", Version: 1,
		DSLVersion: "3.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle: "web", Start: "only",
			Tasks: []apiv1.Task{{
				Name: "only", Type: apiv1.TaskDeterministic,
				Goal: "only",
				Run: &apiv1.DeterministicRun{
					Command:   []string{"true"},
					Workspace: apiv1.WorkspaceScratch,
				},
			}},
		},
		RepoRef: apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub,
			Owner:    "fixture", Name: "repo", Branch: "main",
		},
		TriggerKind:            string(journal.TriggerManual),
		PreviewFeaturesEnabled: &preview,
		LiveJournal:            true,
	}
}

func TestIntegrationCancelWaitsForExecutorOutcome(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	// never-assert-wall-clock: the server context is a 90s hang guard, not
	// a cancellation latency SLO. Assert journal ordering below instead.
	ctx, server := cancellationDevServer(t)
	c := server.Client()
	for _, test := range []struct {
		name         string
		acknowledge  bool
		wantFinished int
	}{
		{name: "committed-success", wantFinished: 1},
		{name: "acknowledged-cancel", acknowledge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			queue := "cancel-outcome-" + test.name
			executor := &cancelOutcomeExecutor{
				acknowledge: test.acknowledge,
				started:     make(chan struct{}),
				release:     make(chan struct{}),
				cancelSeen:  make(chan struct{}),
			}
			writer, runsDir := newLiveWriter(t)
			w := temporalworker.New(c, queue, temporalworker.Options{})
			RegisterWith(w, &Activities{
				Det: executor, Workspaces: testWorkspaces(t), Journal: writer,
			})
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			runID := "cancel-outcome-" + test.name
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID: runID, TaskQueue: queue,
			}, Run, cancelOutcomeRunInput(runID))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-executor.started:
			case <-ctx.Done():
				t.Fatal("executor did not start:", ctx.Err())
			}
			if err := c.CancelWorkflow(
				ctx, run.GetID(), run.GetRunID()); err != nil {
				t.Fatal(err)
			}
			if !test.acknowledge {
				close(executor.release)
			}
			if err := run.Get(ctx, nil); err == nil ||
				!strings.Contains(err.Error(), "canceled") {
				t.Fatalf("workflow error = %v, want canceled", err)
			}
			description, err := c.DescribeWorkflowExecution(
				ctx, run.GetID(), run.GetRunID())
			if err != nil {
				t.Fatal(err)
			}
			if status := description.WorkflowExecutionInfo.Status; status != enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED {
				t.Fatalf("workflow status = %s, want CANCELED", status)
			}
			if test.acknowledge {
				select {
				case <-executor.cancelSeen:
				default:
					t.Fatal("non-heartbeating executor never observed ctx.Done")
				}
			}
			events := liveEvents(t, runsDir, runID)
			finished, terminals := 0, 0
			for _, event := range events {
				if event.Type == journal.EventStageFinished {
					if terminals != 0 {
						t.Fatalf("stage.finished recorded after run.finished: %+v", events)
					}
					finished++
				}
				if event.Type == journal.EventRunFinished {
					terminals++
					if event.Status != string(journal.PhaseAborted) || finished != test.wantFinished {
						t.Fatalf("run.finished=%+v after %d stage.finished, want aborted after %d",
							event, finished, test.wantFinished)
					}
				}
				if event.Type == journal.EventError && event.Error != nil &&
					event.Error.Code == JournalEmitFailedErrorCode {
					t.Fatalf("cancellation emitted infra noise: %+v", event)
				}
			}
			if terminals != 1 {
				t.Fatalf("run.finished=%d, want 1; events=%+v", terminals, events)
			}
			if finished != test.wantFinished {
				t.Fatalf("stage.finished=%d, want %d; events=%+v",
					finished, test.wantFinished, events)
			}
		})
	}
}

func TestIntegrationWorkerStopDoesNotBecomeOperatorCancel(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, server := cancellationDevServer(t)
	c := server.Client()
	const queue = "cancel-outcome-worker-stop"
	executor := &cancelOutcomeExecutor{
		acknowledge: true,
		started:     make(chan struct{}),
		release:     make(chan struct{}),
		cancelSeen:  make(chan struct{}),
	}
	writer, runsDir := newLiveWriter(t)
	workerA := temporalworker.New(c, queue, temporalworker.Options{
		Identity:          "worker-a",
		WorkerStopTimeout: time.Second,
	})
	RegisterWith(workerA, &Activities{
		Det: executor, Workspaces: testWorkspaces(t), Journal: writer,
	})
	if err := workerA.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if workerA != nil {
			workerA.Stop()
		}
	})
	const runID = "cancel-outcome-worker-stop"
	input := cancelOutcomeRunInput(runID)
	input.Spec.Tasks[0].Retry = &apiv1.RetryPolicy{MaxAttempts: 2}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: runID, TaskQueue: queue,
	}, Run, input)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-executor.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	workerB := temporalworker.New(c, queue, temporalworker.Options{
		Identity: "worker-b",
	})
	RegisterWith(workerB, &Activities{
		Det: &fakeRunner{run: func(
			context.Context,
			apiv1.InvocationEnvelope,
			apiv1.DeterministicRun,
		) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		}},
		Workspaces: testWorkspaces(t),
		Journal:    writer,
	})
	if err := workerB.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(workerB.Stop)
	workerA.Stop()
	workerA = nil
	select {
	case <-executor.cancelSeen:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := run.Get(ctx, nil); err != nil {
		t.Fatalf("worker stop became a terminal workflow error: %v", err)
	}
	description, err := c.DescribeWorkflowExecution(
		ctx, run.GetID(), run.GetRunID())
	if err != nil {
		t.Fatal(err)
	}
	if status := description.WorkflowExecutionInfo.Status; status != enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		t.Fatalf("workflow status = %s, want COMPLETED", status)
	}
	finished := 0
	for _, event := range liveEvents(t, runsDir, runID) {
		if event.Type == journal.EventStageFinished {
			finished++
		}
		if event.Type == journal.EventRunFinished &&
			(event.Status == string(journal.PhaseAborted) ||
				event.TerminalCause != nil &&
					event.TerminalCause.Classification ==
						journal.TerminalOperatorAbort) {
			t.Fatalf("worker stop became operator abort: %+v", event)
		}
	}
	if finished != 1 {
		t.Fatalf("stage.finished=%d, want 1", finished)
	}
}
