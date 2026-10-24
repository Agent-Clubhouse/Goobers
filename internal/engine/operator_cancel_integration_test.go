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

func TestIntegrationCancelWaitsForExecutorOutcome(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
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
			preview := true
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID: runID, TaskQueue: queue,
			}, Run, RunInput{
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
			})
			if err != nil {
				t.Fatal(err)
			}
			<-executor.started
			cancelStarted := time.Now()
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
			if elapsed := time.Since(cancelStarted); elapsed >= 2*dispatchHeartbeatTimeout {
				t.Fatalf("cancel-to-terminal = %s, want <%s",
					elapsed, 2*dispatchHeartbeatTimeout)
			}
			if test.acknowledge {
				select {
				case <-executor.cancelSeen:
				default:
					t.Fatal("non-heartbeating executor never observed ctx.Done")
				}
			}
			events := liveEvents(t, runsDir, runID)
			finished := 0
			for _, event := range events {
				if event.Type == journal.EventStageFinished {
					finished++
				}
				if event.Type == journal.EventError && event.Error != nil &&
					event.Error.Code == JournalEmitFailedErrorCode {
					t.Fatalf("cancellation emitted infra noise: %+v", event)
				}
			}
			if finished != test.wantFinished {
				t.Fatalf("stage.finished=%d, want %d; events=%+v",
					finished, test.wantFinished, events)
			}
		})
	}
}
