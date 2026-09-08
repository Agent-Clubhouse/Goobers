package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/temporaltest"
)

// Exercise the real activity error serialization, retry loop, and terminal
// journal: classification must survive a Temporal boundary without parsing text.
func TestDispatchFailureHealthClassification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures []error
		fail     bool
		count    int
		class    string
	}{
		{"recovered", []error{invoke.InfrastructureFailure(errors.New("pod vanished"))}, false, 2, "infra"},
		{"exhausted", persistentFailures(invoke.InfrastructureFailure(errors.New("pod vanished")), 100), true, int(runner.DefaultMaxInfrastructureAttempts), "infra"},
		{"policy", []error{errors.New("GoobersInfrastructureFailure is only text")}, true, 1, "executor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			det := &scriptedDeterministic{failures: tc.failures}
			proj := executeForProjection(t, runInput("health-"+tc.name, retrySpec(nil)), &Activities{Det: det, Workspaces: testWorkspaces(t)}, tc.fail)
			if det.callCount() != tc.count {
				t.Fatalf("dispatches = %d, want %d", det.callCount(), tc.count)
			}
			var first, terminal *journal.Event
			var success *journal.Event
			for _, op := range proj.Ops {
				e := op.Event
				if e == nil {
					continue
				}
				if e.Type == journal.EventError && e.Error.Code == "executor_error" && first == nil {
					first = e
				}
				if e.Type == journal.EventError && e.Error.Code == "run_failed" {
					terminal = e
				}
				if e.Type == journal.EventStageFinished && e.Status == "success" {
					success = e
				}
			}
			if first == nil || first.AttemptClass != "" || first.Attempt != 1 || first.Runner["errorClass"] != tc.class {
				t.Fatalf("initial failure = %+v, want initial attempt with structured %s outcome", first, tc.class)
			}
			expectedCode := telemetry.ErrCodeExecutor
			if tc.class == "infra" {
				expectedCode = telemetry.ErrCodeInfraFailure
			}
			if first.Runner["errorCode"] != expectedCode {
				t.Fatalf("failure metadata = %+v", first.Runner)
			}
			if tc.fail {
				if terminal == nil || terminal.Runner["errorClass"] != tc.class {
					t.Fatalf("terminal = %+v, want %s", terminal, tc.class)
				}
				if _, exists := terminal.Runner["errorCode"]; exists {
					t.Fatal("terminal refinement must preserve the run_failed rollup row")
				}
			} else if terminal != nil || success == nil || success.AttemptClass != journal.AttemptInfra {
				t.Fatalf("recovery terminal=%+v success=%+v", terminal, success)
			}
		})
	}
}

// Capture the real Run return before the test environment wraps it, then cross
// the SDK failure boundary exactly as a failed workflow completion does.
func TestFullRunTerminalInfrastructureSurvivesFailureSerialization(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{Det: &scriptedDeterministic{failures: persistentFailures(invoke.InfrastructureFailure(errors.New("pod vanished")), 100)}, Workspaces: testWorkspaces(t)})
	var returned error
	env.ExecuteWorkflow(func(ctx workflow.Context, in RunInput) (RunResult, error) {
		result, err := Run(ctx, in)
		returned = err
		return result, err
	}, runInput("serialized-terminal", retrySpec(nil)))
	if env.GetWorkflowError() == nil || returned == nil {
		t.Fatal("full Run did not fail at its infrastructure budget")
	}
	converter := temporal.GetDefaultFailureConverter()
	wireErr := converter.FailureToError(converter.ErrorToFailure(returned))
	if class, err := ClassifyDispatchFailure(wireErr); err != nil || class != journal.AttemptInfra {
		t.Fatalf("serialized full Run failure class=%q err=%v: %v", class, err, wireErr)
	}
	if !strings.Contains(wireErr.Error(), fmt.Sprintf("attempt %d/%d", runner.DefaultMaxInfrastructureAttempts, runner.DefaultMaxInfrastructureAttempts)) {
		t.Fatalf("exhaustion context lost: %v", wireErr)
	}
	policy := temporal.NewApplicationErrorWithCause("explicit policy refusal", FailureTypeStage, temporal.NewApplicationError("inner outage", FailureTypeInfrastructure))
	wirePolicy := converter.FailureToError(converter.ErrorToFailure(terminalWorkflowFailure(policy)))
	if class, err := ClassifyDispatchFailure(wirePolicy); err != nil || class != journal.AttemptPolicy {
		t.Fatalf("explicit policy wrapper promoted: class=%q err=%v", class, err)
	}
}
