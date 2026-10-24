package schedstart

import (
	"context"
	"errors"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/frame"
)

// Workflows is the Temporal surface the terminal wait needs; client.Client
// satisfies it (and the terminal awaiter's own Workflows interface).
type Workflows interface {
	GetWorkflow(ctx context.Context, workflowID, runID string) client.WorkflowRun
}

// TemporalTerminal waits for the engine workflow of a run to close, the same
// primitive the leader's terminal awaiter uses. The two waits are independent:
// the awaiter fires the terminal hooks and releases claims, this one only
// releases the scheduler slot.
//
// What counts as terminal is exact, because a wrong "terminal" frees the slot of
// a live run and a wrong "transient" holds a dead run's slot forever:
//   - a result: the run's own outcome (frame.PhaseFor);
//   - a workflow-execution error (failed, canceled, terminated, timed out): the
//     run is closed and failed;
//   - NotFound: the execution is permanently gone, closed and failed;
//   - anything else (an unavailable server, a deadline) is transient: it is
//     returned so awaitTerminal retries, and it never frees the slot.
func TemporalTerminal(workflows Workflows) Terminal {
	return func(ctx context.Context, runID string) (Outcome, error) {
		var result engine.RunResult
		err := workflows.GetWorkflow(ctx, runID, "").Get(ctx, &result)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Outcome{}, ctxErr
		}
		if err == nil {
			return Outcome{
				Phase: frame.PhaseFor(result), NoWork: result.NoWork,
				FailureCode: result.FailureCode, FailureMessage: result.FailureMessage,
			}, nil
		}
		var execution *temporal.WorkflowExecutionError
		var notFound *serviceerror.NotFound
		if errors.As(err, &execution) || errors.As(err, &notFound) {
			return Outcome{Phase: journal.PhaseFailed}, nil
		}
		return Outcome{}, err
	}
}
