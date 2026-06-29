package engine

import (
	"context"
	"errors"
	"strings"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// StartResult reports the outcome of starting a run.
type StartResult struct {
	// RunID is the run/workflow id the run executes under.
	RunID string
	// AlreadyRunning is true when a run with the same id was already in flight,
	// so this Start was a no-op. This is the engine's exactly-once guarantee: a
	// deterministic RunID (e.g. one per backlog item) makes a duplicate Start
	// idempotent rather than launching a second run.
	AlreadyRunning bool
}

// Starter is the engine's start API: it begins a workflow run for a pinned
// RunInput. The scheduler (M11) depends on this; the runtime/operator provides a
// Temporal-backed implementation.
type Starter interface {
	Start(ctx context.Context, in RunInput) (StartResult, error)
}

// RunID builds a deterministic run id from its non-empty parts joined by "/".
// Using the same parts (e.g. gaggle, workflow, item id) yields the same id, so a
// second Start for the same unit of work is rejected as already-running.
func RunID(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return strings.Join(cleaned, "/")
}

// workflowStarter is the slice of the Temporal client the TemporalStarter needs.
// client.Client satisfies it; tests provide a fake.
type workflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// TemporalStarter starts engine runs on a Temporal task queue. It sets the run's
// WorkflowID to RunInput.RunID and asks Temporal to error if that id is already
// running, which it maps to a non-error AlreadyRunning result.
type TemporalStarter struct {
	client    workflowStarter
	taskQueue string
}

// NewTemporalStarter builds a TemporalStarter for the given Temporal client and
// task queue.
func NewTemporalStarter(c client.Client, taskQueue string) *TemporalStarter {
	return &TemporalStarter{client: c, taskQueue: taskQueue}
}

// Start launches the engine workflow for in, idempotently on in.RunID.
func (s *TemporalStarter) Start(ctx context.Context, in RunInput) (StartResult, error) {
	if in.RunID == "" {
		return StartResult{}, errors.New("engine: RunInput.RunID is required to start a run")
	}
	opts := client.StartWorkflowOptions{
		ID:                                       in.RunID,
		TaskQueue:                                s.taskQueue,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}
	run, err := s.client.ExecuteWorkflow(ctx, opts, Run, in)
	if err != nil {
		if isAlreadyStarted(err) {
			return StartResult{RunID: in.RunID, AlreadyRunning: true}, nil
		}
		return StartResult{}, err
	}
	return StartResult{RunID: run.GetID()}, nil
}

// isAlreadyStarted reports whether err is Temporal's "already started" error.
func isAlreadyStarted(err error) bool {
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(err, &already)
}
