// Package temporaltest provides the shared constructor for Temporal SDK test
// workflow environments used across the engine and end-to-end suites.
//
// It exists to hold one decision in one place: the deadlock-detection budget
// (#1735). The SDK arms a watchdog that fails a workflow task if the workflow
// goroutine does not yield within a timeout, defaulting to one second. That
// budget assumes a workflow goroutine performs only bookkeeping between yields,
// which is true in production. It is not a safe assumption for a test binary
// running under -race on a shared CI runner, where the race detector's
// instrumentation and the scheduler contention from parallel shards routinely
// stretch a sub-millisecond stretch of bookkeeping past a full second. The
// result is a workflow that is not deadlocked being reported as deadlocked.
//
// Rather than disable detection, callers get a budget wide enough that runner
// contention cannot reach it while still being far below the go test timeout,
// so a genuine non-yielding workflow is still caught and still fails the suite.
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

// NewWorkflowEnvironment returns a test workflow environment from ts with the
// deadlock-detection budget set to DeadlockDetectionTimeout.
//
// Prefer this over calling ts.NewTestWorkflowEnvironment directly: an
// environment built without the budget inherits the SDK's one-second default
// and is flaky under -race on CI.
func NewWorkflowEnvironment(ts *testsuite.WorkflowTestSuite) *testsuite.TestWorkflowEnvironment {
	env := ts.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{DeadlockDetectionTimeout: DeadlockDetectionTimeout})
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
