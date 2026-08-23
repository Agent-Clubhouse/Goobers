package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
)

// scriptedDeterministic fails with each scripted error in order, then
// succeeds — the engine-side analogue of the local runner's
// sequencedDeterministic (internal/runner/run_test.go), so retry-budget
// assertions can be ported one for one.
type scriptedDeterministic struct {
	mu       sync.Mutex
	failures []error
	calls    int
}

func (s *scriptedDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= len(s.failures) {
		return apiv1.ResultEnvelope{}, s.failures[s.calls-1]
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "eventually succeeded"}, nil
}

func (s *scriptedDeterministic) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// retrySpec is a single deterministic task with the given retry policy —
// the engine-side mirror of the local runner's retryFixtureMachine.
func retrySpec(retry *apiv1.RetryPolicy) apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
			Run:   &apiv1.DeterministicRun{Command: []string{"true"}},
			Retry: retry,
		}},
	}
}

func executeRetryWorkflow(t *testing.T, name string, retry *apiv1.RetryPolicy, det invoke.Deterministic) error {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(Run, runInput(name, retrySpec(retry)))
	return env.GetWorkflowError()
}

// TestTaskWithoutRetryGetsExactlyOnePolicyAttempt proves Temporal's
// unlimited-attempts default is never in effect: a task with no retry block
// dispatches exactly once on a policy-class failure — the local runner's
// policyMaxAttempts=1 default, not infinity (#622).
func TestTaskWithoutRetryGetsExactlyOnePolicyAttempt(t *testing.T) {
	det := &scriptedDeterministic{failures: persistentFailures(errors.New("business bug"), 100)}
	err := executeRetryWorkflow(t, "no-retry", nil, det)
	if err == nil || !strings.Contains(err.Error(), "(attempt 1/1)") {
		t.Fatalf("workflow error = %v, want exhaustion at attempt 1/1", err)
	}
	if got := det.callCount(); got != 1 {
		t.Fatalf("dispatches = %d, want exactly 1 (no retry block, policy failure)", got)
	}
}

// TestTaskRetryPolicyBoundsAndRecovers: Task.Retry's budget is honored — a
// task failing policy-class twice under MaxAttempts=3 succeeds on its third
// attempt.
func TestTaskRetryPolicyBoundsAndRecovers(t *testing.T) {
	det := &scriptedDeterministic{failures: []error{
		errors.New("transient failure (call 1)"),
		errors.New("transient failure (call 2)"),
	}}
	err := executeRetryWorkflow(t, "retry-recovers", &apiv1.RetryPolicy{MaxAttempts: 3, BackoffSeconds: 5}, det)
	if err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if got := det.callCount(); got != 3 {
		t.Fatalf("dispatches = %d, want 3 (two policy retries, then success)", got)
	}
}

// TestTaskRetryPolicyExhaustionFailsRun: exhausting the declared policy
// budget fails the run with the same attempt-count accounting as the local
// runner.
func TestTaskRetryPolicyExhaustionFailsRun(t *testing.T) {
	det := &scriptedDeterministic{failures: persistentFailures(errors.New("still broken"), 100)}
	err := executeRetryWorkflow(t, "retry-exhausted", &apiv1.RetryPolicy{MaxAttempts: 2}, det)
	if err == nil || !strings.Contains(err.Error(), "(attempt 2/2)") {
		t.Fatalf("workflow error = %v, want exhaustion at attempt 2/2", err)
	}
	if got := det.callCount(); got != 2 {
		t.Fatalf("dispatches = %d, want 2", got)
	}
}

// TestInfrastructureFailuresBoundedSeparately: infra-marked failures consume
// the shared runner.DefaultMaxInfrastructureAttempts budget — bounded even
// with no retry block, and never borrowing from the policy budget.
func TestInfrastructureFailuresBoundedSeparately(t *testing.T) {
	t.Run("persistent outage fails at the infrastructure bound", func(t *testing.T) {
		det := &scriptedDeterministic{failures: persistentFailures(invoke.InfrastructureFailure(errors.New("503 upstream")), 100)}
		err := executeRetryWorkflow(t, "infra-persistent", nil, det)
		want := fmt.Sprintf("(attempt %d/%d)", runner.DefaultMaxInfrastructureAttempts, runner.DefaultMaxInfrastructureAttempts)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("workflow error = %v, want exhaustion at %s", err, want)
		}
		if !strings.Contains(err.Error(), "503") {
			t.Fatalf("workflow error = %v, want the original cause preserved", err)
		}
		if got := det.callCount(); int32(got) != runner.DefaultMaxInfrastructureAttempts {
			t.Fatalf("dispatches = %d, want %d", got, runner.DefaultMaxInfrastructureAttempts)
		}
	})

	t.Run("transient recovery does not consume the policy budget", func(t *testing.T) {
		det := &scriptedDeterministic{failures: []error{invoke.InfrastructureFailure(errors.New("provider hiccup"))}}
		err := executeRetryWorkflow(t, "infra-recovers", nil, det)
		if err != nil {
			t.Fatalf("workflow error: %v", err)
		}
		if got := det.callCount(); got != 2 {
			t.Fatalf("dispatches = %d, want 2 (one infra recovery on a task whose policy budget is 1)", got)
		}
	})
}

func TestInfrastructureRetryWaitsUntilDeclaredReset(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 0, 0, 0, time.UTC)
	retryAt := now.Add(17 * time.Minute)
	err := classifySeamError(invoke.InfrastructureFailureUntil(errors.New("rate limited"), retryAt))
	if got := infrastructureRetryDelay(err, 5*time.Second, now); got != 17*time.Minute {
		t.Fatalf("retry delay = %v, want 17m", got)
	}
}

// TestMixedInfraAndPolicyBudgets ports the local runner's mixed-failure
// accounting: infra retries never consume policy attempts, and vice versa.
func TestMixedInfraAndPolicyBudgets(t *testing.T) {
	t.Run("infra then policy then success", func(t *testing.T) {
		det := &scriptedDeterministic{failures: []error{
			invoke.InfrastructureFailure(errors.New("worker lost")),
			errors.New("business failure"),
		}}
		err := executeRetryWorkflow(t, "mixed-recovers", &apiv1.RetryPolicy{MaxAttempts: 2}, det)
		if err != nil {
			t.Fatalf("workflow error: %v", err)
		}
		if got := det.callCount(); got != 3 {
			t.Fatalf("dispatches = %d, want 3 (infra retry + policy retry + success)", got)
		}
	})

	t.Run("policy budget still exhausts", func(t *testing.T) {
		det := &scriptedDeterministic{failures: []error{
			invoke.InfrastructureFailure(errors.New("worker lost")),
			errors.New("business failure"),
			errors.New("business failure"),
		}}
		err := executeRetryWorkflow(t, "mixed-exhausts", &apiv1.RetryPolicy{MaxAttempts: 2}, det)
		if err == nil || !strings.Contains(err.Error(), "(attempt 2/2)") {
			t.Fatalf("workflow error = %v, want policy exhaustion at attempt 2/2", err)
		}
		if got := det.callCount(); got != 3 {
			t.Fatalf("dispatches = %d, want 3", got)
		}
	})
}

// TestTransientWorkspaceProvisioningIsInfrastructure mirrors #572: a
// transient worktree-provision failure marked at the invoke seam flows
// through the bounded infrastructure budget without the executor ever
// running, and a second provision succeeds.
func TestTransientWorkspaceProvisioningIsInfrastructure(t *testing.T) {
	workspaces := testWorkspaces(t)
	workspaces.provisionErrs = []error{invoke.InfrastructureFailure(errors.New("clone: 503 Service Unavailable"))}
	det := &scriptedDeterministic{}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Det: det, Workspaces: workspaces})
	env.ExecuteWorkflow(Run, runInput("workspace-flaky", retrySpec(nil)))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if got := det.callCount(); got != 1 {
		t.Fatalf("executor dispatches = %d, want 1 (the provisioning failure never reached the executor)", got)
	}
	if got := len(workspaces.provisioned()); got != 1 {
		t.Fatalf("successful provisions = %d, want 1", got)
	}
}

// TestStageActivityOptionsAlwaysCarryExplicitRetryPolicy: every activity
// dispatch carries an explicit single-attempt RetryPolicy, for every task
// shape — Temporal's unlimited default is structurally unreachable (#622).
func TestStageActivityOptionsAlwaysCarryExplicitRetryPolicy(t *testing.T) {
	for _, limits := range []apiv1.Limits{{}, {MaxDurationSeconds: 90}} {
		opts := stageActivityOptions(limits, "")
		if opts.RetryPolicy == nil {
			t.Fatalf("limits %+v: RetryPolicy is nil — Temporal's unlimited default would apply", limits)
		}
		if opts.RetryPolicy.MaximumAttempts != 1 {
			t.Fatalf("limits %+v: MaximumAttempts = %d, want 1 (retry orchestration lives in dispatchWithRetry)", limits, opts.RetryPolicy.MaximumAttempts)
		}
	}
}

// TestStageActivityTimeoutRunsGraceBehindDeclaredLimit: a declared duration
// limit becomes StartToCloseTimeout only after stageTimeoutGrace padding, so
// the worker's own policy-classed enforcement of the limit (invoke.Timeout →
// FailureTypeStage, the local runner's class) always beats Temporal's
// worker-loss timeout; an undeclared limit keeps the constant default.
func TestStageActivityTimeoutRunsGraceBehindDeclaredLimit(t *testing.T) {
	if got := stageActivityOptions(apiv1.Limits{}, "").StartToCloseTimeout; got != activityTimeout {
		t.Fatalf("undeclared limit StartToCloseTimeout = %v, want %v", got, activityTimeout)
	}
	want := 90*time.Second + stageTimeoutGrace
	if got := stageActivityOptions(apiv1.Limits{MaxDurationSeconds: 90}, "").StartToCloseTimeout; got != want {
		t.Fatalf("declared limit StartToCloseTimeout = %v, want %v (limit + grace, so the worker-side policy timeout wins the race)", got, want)
	}
}

// TestAttemptFailureClass covers the class derivation the projection (#629)
// reuses: infra-typed and stage-typed application errors, Temporal timeouts,
// and the unclassifiable-fails-closed case.
func TestAttemptFailureClass(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantClass journal.AttemptClass
		wantErr   bool
	}{
		{"infrastructure-typed application error", temporal.NewApplicationError("503", FailureTypeInfrastructure), journal.AttemptInfra, false},
		{"stage-typed application error", temporal.NewApplicationError("boom", FailureTypeStage), journal.AttemptPolicy, false},
		{"untyped application error is policy (unmarked means policy)", temporal.NewApplicationError("boom", ""), journal.AttemptPolicy, false},
		{"schedule-to-start timeout is infrastructure", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START, nil), journal.AttemptInfra, false},
		{"start-to-close timeout is policy", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil), journal.AttemptPolicy, false},
		{"schedule-to-start timeout overrides nested stage error", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START, temporal.NewApplicationError("boom", FailureTypeStage)), journal.AttemptInfra, false},
		{"start-to-close timeout overrides nested infrastructure error", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, temporal.NewApplicationError("503", FailureTypeInfrastructure)), journal.AttemptPolicy, false},
		{"anything else fails closed", errors.New("mystery"), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, err := attemptFailureClass(tc.err)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "unclassifiable") {
					t.Fatalf("err = %v, want the unclassifiable fail-closed error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("attemptFailureClass: %v", err)
			}
			if class != tc.wantClass {
				t.Fatalf("class = %q, want %q", class, tc.wantClass)
			}
		})
	}
}

func TestSideEffectingStageTimeoutRedispatch(t *testing.T) {
	tests := []struct {
		name          string
		timeoutType   enumspb.TimeoutType
		wantCalls     int
		wantErr       bool
		wantErrSubstr string
	}{
		{
			name:        "schedule-to-start retries because activity never began",
			timeoutType: enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START,
			wantCalls:   2,
		},
		{
			name:          "start-to-close does not retry after possible effect",
			timeoutType:   enumspb.TIMEOUT_TYPE_START_TO_CLOSE,
			wantCalls:     1,
			wantErr:       true,
			wantErrSubstr: "refusing to retry after worker loss",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			var attempts []int
			var ts testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&ts)
			env.ExecuteWorkflow(func(ctx workflow.Context) error {
				task := retrySpec(&apiv1.RetryPolicy{MaxAttempts: 3}).Tasks[0]
				task.PolicyActions = []string{"external-mutation"}
				_, err := dispatchWithRetry(ctx, task, &runJournal{}, nil, func(_ workflow.Context, attempt int) (stageActivityResult, error) {
					calls++
					attempts = append(attempts, attempt)
					if calls == 1 {
						return stageActivityResult{}, temporal.NewTimeoutError(test.timeoutType, nil)
					}
					return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
				})
				return err
			})

			err := env.GetWorkflowError()
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), test.wantErrSubstr) {
					t.Fatalf("workflow error = %v, want %q", err, test.wantErrSubstr)
				}
			} else if err != nil {
				t.Fatalf("workflow error: %v", err)
			}
			if calls != test.wantCalls {
				t.Fatalf("dispatches = %d, want %d", calls, test.wantCalls)
			}
			for i, attempt := range attempts {
				if attempt != i+1 {
					t.Fatalf("attempt sequence = %v, want 1..%d", attempts, len(attempts))
				}
			}
		})
	}
}

func TestNormalizeArtifactIntegrityUsesRunnerOwnedAgenticGrade(t *testing.T) {
	tests := []struct {
		name     string
		taskType apiv1.TaskType
		input    apiv1.Integrity
		want     apiv1.Integrity
	}{
		{name: "agent cannot claim trusted", taskType: apiv1.TaskAgentic, input: apiv1.IntegrityTrusted, want: apiv1.IntegrityDerived},
		{name: "provider grade survives deterministic stage", taskType: apiv1.TaskDeterministic, input: apiv1.IntegrityUnapproved, want: apiv1.IntegrityUnapproved},
		{name: "missing deterministic grade defaults closed", taskType: apiv1.TaskDeterministic, want: apiv1.IntegrityDerived},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifacts := normalizeArtifactIntegrity(test.taskType, []apiv1.ArtifactPointer{{Integrity: test.input}})
			if got := artifacts[0].Integrity; got != test.want {
				t.Fatalf("integrity = %q, want %q", got, test.want)
			}
		})
	}
}

func persistentFailures(err error, n int) []error {
	out := make([]error, n)
	for i := range out {
		out[i] = err
	}
	return out
}
