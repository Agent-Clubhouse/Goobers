package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// ActCommitTerminalIntervention names the trusted worker journal activity.
const ActCommitTerminalIntervention = "CommitTerminalIntervention"

// TerminalInterventionCommitter is available only to a resident trusted worker.
// Stage pod emitters cannot mint this seam's distinct credential.
type TerminalInterventionCommitter interface {
	CommitTerminalIntervention(context.Context, livejournal.TerminalInterventionRequest) (journal.TerminalInterventionReceipt, error)
}

// CommitTerminalIntervention classifies definitive refusals separately from
// uncertain delivery, which Temporal retries with the same keyed payload.
func (a *Activities) CommitTerminalIntervention(ctx context.Context, req livejournal.TerminalInterventionRequest) (journal.TerminalInterventionReceipt, error) {
	committer, ok := a.Journal.(TerminalInterventionCommitter)
	if !ok {
		return journal.TerminalInterventionReceipt{}, temporal.NewNonRetryableApplicationError("worker has no terminal journal seam", HITLErrNotAcceptingUp, nil)
	}
	receipt, err := committer.CommitTerminalIntervention(ctx, req)
	if err != nil {
		switch {
		case errors.Is(err, livejournal.ErrTerminalAuthority):
			return receipt, temporal.NewNonRetryableApplicationError(err.Error(), HITLErrUnauthorized, nil)
		case errors.Is(err, journal.ErrInterventionStale):
			return receipt, temporal.NewNonRetryableApplicationError(err.Error(), HITLErrGeneration, nil)
		case errors.Is(err, journal.ErrInterventionConflict), errors.Is(err, journal.ErrInterventionPending), errors.Is(err, journal.ErrInterventionCancelled):
			return receipt, temporal.NewNonRetryableApplicationError(err.Error(), HITLErrKeyReused, nil)
		case errors.Is(err, journal.ErrInterventionInvalid):
			return receipt, temporal.NewNonRetryableApplicationError(err.Error(), HITLErrInvalidIntent, nil)
		default:
			return receipt, fmt.Errorf("terminal journal delivery: %w", err)
		}
	}
	if receipt.Seq == 0 || receipt.Segment == 0 || receipt.Cancelled != (len(req.Transition.Cancellation) > 0) {
		return journal.TerminalInterventionReceipt{}, errors.New("terminal journal returned an incomplete receipt")
	}
	return receipt, nil
}

// All uncertain replies retry the identical activity payload server-side. This
// keeps one Update pending without adding a command to history per retry.
func terminalInterventionContext(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: emitAttemptTimeout,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 30 * time.Second},
	})
}
