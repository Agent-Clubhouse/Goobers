package engine

import (
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/temporaltest"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestEngineRetryBackoffPreservesTimerAndLegacyHistory(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			if legacy {
				env.OnGetVersion(retryBackoffObservationChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			det := &scriptedDeterministic{failures: []error{errors.New("retryable policy failure")}}
			env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t)})
			env.ExecuteWorkflow(Run, runInput("observed-retry", retrySpec(&apiv1.RetryPolicy{MaxAttempts: 2, BackoffSeconds: 5})))
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			if det.callCount() != 2 {
				t.Fatalf("budget changed: %d", det.callCount())
			}
			value, err := env.QueryWorkflow(JournalQuery)
			if err != nil {
				t.Fatal(err)
			}
			var projection JournalProjection
			if err := value.Get(&projection); err != nil {
				t.Fatal(err)
			}
			var state readmodel.RetryBackoffState
			var failedAt, secondAt, deadline time.Time
			count := 0
			for _, op := range projection.Ops {
				if op.Event == nil {
					continue
				}
				event := *op.Event
				event.Schema, event.Time = journal.EventSchema, op.Time
				state = state.After(event)
				if event.Type == journal.EventError && event.Stage == "implement" && event.Attempt == 1 {
					failedAt = op.Time
				}
				if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == journal.RetryBackoffKind {
					count++
					if len(state.Waits) != 1 {
						t.Fatalf("unprojectable timer: %+v", event)
					}
					wait := state.Waits[0]
					if wait.Driver != "engine" || wait.Class != journal.AttemptPolicy || wait.Attempt != 1 || !wait.ObservedAt.Equal(op.Time) || wait.Deadline.Sub(wait.ObservedAt) != 5*time.Second {
						t.Fatalf("timer=%+v", wait)
					}
					deadline = wait.Deadline
				}
				if event.Type == journal.EventStageStarted && event.Stage == "implement" && event.Attempt == 2 {
					secondAt = op.Time
					if len(state.Waits) != 0 {
						t.Fatal("next attempt retained wait")
					}
				}
			}
			want := 1
			if legacy {
				want = 0
			}
			if count != want || len(state.Waits) != 0 {
				t.Fatalf("count=%d terminal=%+v", count, state)
			}
			if failedAt.IsZero() || secondAt.Sub(failedAt) != 5*time.Second {
				t.Fatalf("scheduled delay changed: failure=%v retry=%v", failedAt, secondAt)
			}
			if !legacy && !deadline.Equal(secondAt) {
				t.Fatalf("deadline=%v retry=%v", deadline, secondAt)
			}
		})
	}
}
