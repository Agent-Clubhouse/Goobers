package temporaltest_test

import (
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
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

// sdkDefaultTestIdleTimeout is the SDK's own testTimeout default, restated here
// because it is unexported. It is the budget TestIdleTimeout exists to widen,
// and the reason slowActivity sleeps for longer than it.
const sdkDefaultTestIdleTimeout = 3 * time.Second

// slowActivity keeps the test environment's main loop idle — no callbacks to
// process — for longer than the SDK's default budget. It stands in for the
// activities the engine suites actually run: writing a live journal, staging a
// workspace, driving the parity harness's scripted stages. None of those is
// slow by design; they become slow because the machine is busy.
func slowActivity() (string, error) {
	time.Sleep(sdkDefaultTestIdleTimeout + time.Second)
	return "activity done", nil
}

// activityWorkflow blocks on slowActivity, which is what makes the main loop
// idle. The workflow goroutine itself yields immediately, so this exercises the
// idle budget and NOT the deadlock budget — the two are separate settings and
// only one of them was pinned before #3874.
func activityWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	var out string
	if err := workflow.ExecuteActivity(ctx, slowActivity).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

// TestNewWorkflowEnvironmentSurvivesAnIdleMainLoop is the regression test for
// the second budget (#3874). It has the same two-sided shape as the deadlock
// test above, for the same reason: the helper must tolerate the delay, and the
// SDK default must be shown to reject it, so a future SDK bump that changes
// either the default or the plumbing reports which one moved.
//
// The failure this prevents is particularly bad to debug: the SDK does not
// return an error, it PANICS on the test's own goroutine with "test timeout:
// 3s, workflow stack: ...", which takes the whole package down and names a
// workflow that was never stuck. It was seen against internal/engine under
// -race with other suites running alongside, and did not reproduce on an
// unloaded run — the signature of a wall-clock budget, not a defect.
func TestNewWorkflowEnvironmentSurvivesAnIdleMainLoop(t *testing.T) {
	t.Run("helper tolerates a long-running activity", func(t *testing.T) {
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		env.RegisterWorkflow(activityWorkflow)
		env.RegisterActivity(slowActivity)

		env.ExecuteWorkflow(activityWorkflow)

		if !env.IsWorkflowCompleted() {
			t.Fatal("workflow did not complete")
		}
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error = %v, want nil (idle budget too small)", err)
		}
		var result string
		if err := env.GetWorkflowResult(&result); err != nil {
			t.Fatalf("result: %v", err)
		}
		if result != "activity done" {
			t.Fatalf("result = %q, want %q", result, "activity done")
		}
	})

	t.Run("SDK default panics on the same workflow", func(t *testing.T) {
		panicked := func() (msg string) {
			defer func() {
				if r := recover(); r != nil {
					msg, _ = r.(string)
				}
			}()
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: temporaltest.DeadlockDetectionTimeout})
			env.RegisterWorkflow(activityWorkflow)
			env.RegisterActivity(slowActivity)
			env.ExecuteWorkflow(activityWorkflow)
			return ""
		}()

		if !strings.Contains(panicked, "test timeout") {
			t.Fatalf("SDK default produced %q, want a \"test timeout\" panic; if this now passes, the SDK "+
				"default changed and TestIdleTimeout may be unnecessary", panicked)
		}
	})
}

// TestTestIdleTimeoutStillDetects is the other direction, exactly as for the
// deadlock budget: an actually idle workflow must still be reported, with its
// stack, rather than being left to expire as an opaque go test timeout.
func TestTestIdleTimeoutStillDetects(t *testing.T) {
	if temporaltest.TestIdleTimeout <= sdkDefaultTestIdleTimeout {
		t.Fatalf("TestIdleTimeout = %v, want > %v (the SDK default it exists to widen)",
			temporaltest.TestIdleTimeout, sdkDefaultTestIdleTimeout)
	}
	if temporaltest.TestIdleTimeout > 2*time.Minute {
		t.Fatalf("TestIdleTimeout = %v, want <= 2m so a truly idle workflow still panics with its stack "+
			"rather than expiring as a suite timeout", temporaltest.TestIdleTimeout)
	}
}
