// Package temporaltest provides the shared constructor for Temporal SDK test
// workflow environments used across the engine and end-to-end suites.
//
// It exists to hold two decisions in one place, both of the same shape: the
// SDK arms wall-clock budgets that are safe in production and unsafe in a test
// binary running under -race on a contended machine, where the race detector's
// instrumentation and scheduler contention from parallel shards routinely
// stretch a sub-millisecond stretch of bookkeeping past a whole second. Left at
// their defaults, both report a healthy workflow as a broken one.
//
// The budgets are DeadlockDetectionTimeout (#1735) and TestIdleTimeout (#3874).
// Rather than disable either check, callers get budgets wide enough that runner
// contention cannot reach them while still being far below the go test timeout,
// so a genuinely stuck workflow is still caught and still fails the suite.
package temporaltest

import (
	"context"
	"time"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
)

// DeadlockDetectionTimeout is the budget a workflow goroutine has to yield
// before the SDK reports a potential deadlock.
//
// The value trades detection latency for freedom from false positives. Real
// deadlocks are unbounded, so any finite budget catches them; the only thing a
// larger budget costs is how long the suite takes to notice. Thirty seconds is
// roughly thirty times the worst yield stall observed under -race on a
// contended runner, and well inside the go test timeout, so a genuinely stuck
// workflow still surfaces as a TMPRL1101 failure rather than a suite timeout.
const DeadlockDetectionTimeout = 30 * time.Second

// TestIdleTimeout is how long the test environment's main loop waits with no
// callback to process before it panics with the workflow's stack.
//
// The SDK defaults this to THREE SECONDS of wall clock (its testTimeout), a
// separate budget from DeadlockDetectionTimeout and one the deadlock setting
// does not cover: the watchdog above asks "did the workflow goroutine yield?",
// while this one asks "did anything at all happen recently?". A fixture whose
// activities do real work — writing a live journal, provisioning a workspace,
// running the parity harness's scripted stages — can idle the loop past three
// seconds purely because the machine is busy, and the panic that follows
// ("test timeout: 3s, workflow stack: ...") names a workflow that was never
// stuck. Observed against internal/engine under -race with the package's own
// suites running alongside it, and reproducibly absent from an unloaded run,
// which is the signature of a wall-clock budget rather than a defect (#3874).
//
// Sixty seconds keeps the check — an actually idle workflow still panics with
// its stack, which is a far better failure than an opaque suite timeout — while
// sitting an order of magnitude above any contention stall and comfortably
// inside go test's own default timeout.
const TestIdleTimeout = 60 * time.Second

// NewWorkflowEnvironment returns a test workflow environment from ts with both
// wall-clock budgets set: DeadlockDetectionTimeout and TestIdleTimeout.
//
// Prefer this over calling ts.NewTestWorkflowEnvironment directly: an
// environment built without them inherits the SDK's one-second and
// three-second defaults and is flaky under -race on CI.
func NewWorkflowEnvironment(ts *testsuite.WorkflowTestSuite) *testsuite.TestWorkflowEnvironment {
	env := ts.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: DeadlockDetectionTimeout})
	env.SetTestTimeout(TestIdleTimeout)
	return env
}

// ProjectionQuerier adapts a TestWorkflowEnvironment's QueryWorkflow to the
// (ctx, workflowID, runID, queryType, args...) shape a real Temporal
// client.Client exposes (#2903). internal/engine's journal-projection helpers
// (ProjectCompletedRun, ProjectCompletedRunForGaggle, and the
// CompletedRunReconciler) are written against that client shape so they work
// unmodified against a live server; this adapter lets the same, unmodified
// projection code be driven hermetically against the SDK test environment
// once a workflow run inside it, connecting the engine-start half (env.
// ExecuteWorkflow) to the completed-run-projection half in one process — the
// callable path an integration test needs without a live Temporal server.
//
// The test environment hosts exactly one workflow at a time, so ctx,
// workflowID, and runID are accepted only for signature compatibility and are
// otherwise ignored.
type ProjectionQuerier struct {
	Env *testsuite.TestWorkflowEnvironment
}

// QueryWorkflow satisfies the query shape engine's projection helpers expect.
func (q ProjectionQuerier) QueryWorkflow(_ context.Context, _, _, queryType string, args ...interface{}) (converter.EncodedValue, error) {
	return q.Env.QueryWorkflow(queryType, args...)
}
