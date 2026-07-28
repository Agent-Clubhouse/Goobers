package temporaltest_test

import (
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/temporaltest"
)

// stallingWorkflow occupies the workflow goroutine without yielding for longer
// than the SDK's one-second default deadlock budget. It stands in for what the
// race detector and a contended CI runner do to an ordinary workflow: stretch a
// stretch of non-yielding bookkeeping past a second of wall time.
func stallingWorkflow(ctx workflow.Context) (string, error) {
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
	}
	return "completed", nil
}

// TestNewWorkflowEnvironmentSurvivesLongWorkflowTask is the regression test for
// #1735. Built through the helper, a workflow that holds its goroutine for well
// over a second still completes; built through the SDK constructor directly, the
// same workflow is killed with TMPRL1101. The second half is what made
// TestWalkingSkeletonWiredPath flaky on CI, so it is asserted here rather than
// left implicit — if a future SDK bump changes the default or the plumbing, this
// test reports which of the two moved.
func TestNewWorkflowEnvironmentSurvivesLongWorkflowTask(t *testing.T) {
	t.Run("helper tolerates a long workflow task", func(t *testing.T) {
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		env.RegisterWorkflow(stallingWorkflow)

		env.ExecuteWorkflow(stallingWorkflow)

		if !env.IsWorkflowCompleted() {
			t.Fatal("workflow did not complete")
		}
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error = %v, want nil (deadlock budget too small)", err)
		}
		var result string
		if err := env.GetWorkflowResult(&result); err != nil {
			t.Fatalf("result: %v", err)
		}
		if result != "completed" {
			t.Fatalf("result = %q, want %q", result, "completed")
		}
	})

	t.Run("SDK default rejects the same workflow", func(t *testing.T) {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		env.RegisterWorkflow(stallingWorkflow)

		env.ExecuteWorkflow(stallingWorkflow)

		if err := env.GetWorkflowError(); err == nil {
			t.Fatal("expected the SDK default deadlock budget to reject a 1.5s workflow task; " +
				"if this now passes, the SDK default changed and NewWorkflowEnvironment may be unnecessary")
		}
	})
}

// TestDeadlockDetectionTimeoutStillDetects guards the other direction: the
// budget is relaxed, not disabled. A genuinely stuck workflow must still be
// caught, so the constant has to stay finite and comfortably under the go test
// timeout rather than growing toward math.MaxInt64.
func TestDeadlockDetectionTimeoutStillDetects(t *testing.T) {
	if temporaltest.DeadlockDetectionTimeout <= time.Second {
		t.Fatalf("DeadlockDetectionTimeout = %v, want > 1s (the SDK default it exists to widen)",
			temporaltest.DeadlockDetectionTimeout)
	}
	if temporaltest.DeadlockDetectionTimeout > time.Minute {
		t.Fatalf("DeadlockDetectionTimeout = %v, want <= 1m so a real deadlock still fails "+
			"as TMPRL1101 rather than as a suite timeout", temporaltest.DeadlockDetectionTimeout)
	}
}
