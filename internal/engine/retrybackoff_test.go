package engine

import (
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/temporaltest"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestEngineRetryBackoffPreservesTimerAndLegacyHistory(t *testing.T) {
	for _, tc := range []struct {
		legacy bool
		class  journal.AttemptClass
	}{{false, journal.AttemptPolicy}, {true, journal.AttemptPolicy}, {false, journal.AttemptInfra}, {true, journal.AttemptInfra}} {
		legacy := tc.legacy
		name := "current"
		if legacy {
			name = "legacy"
		}
		t.Run(name+"-"+string(tc.class), func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			if legacy {
				env.OnGetVersion(retryBackoffObservationChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			failure := errors.New("retryable failure")
			policyAttempts := int32(2)
			if tc.class == journal.AttemptInfra {
				failure = invoke.InfrastructureFailure(failure)
				policyAttempts = 1
			}
			det := &scriptedDeterministic{failures: []error{failure}}
			env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t)})
			env.ExecuteWorkflow(Run, runInput("observed-retry", retrySpec(&apiv1.RetryPolicy{MaxAttempts: policyAttempts, BackoffSeconds: 5})))
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
					if wait.Driver != "engine" || wait.Class != tc.class || wait.Attempt != 1 || !wait.ObservedAt.Equal(op.Time) || wait.Deadline.Sub(wait.ObservedAt) != 5*time.Second {
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
