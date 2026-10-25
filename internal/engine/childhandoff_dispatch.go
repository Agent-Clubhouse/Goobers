package engine

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

const childHandoffFailureType = "GoobersChildHandoffUnconfirmed"

func childHandoffFailure(err error) error {
	return temporal.NewNonRetryableApplicationError("engine: child handoff could not settle safely", childHandoffFailureType, err)
}

func isChildHandoffFailure(err error) bool {
	var application *temporal.ApplicationError
	return errors.As(err, &application) && application.Type() == childHandoffFailureType
}

type childDispatchOutcome struct {
	report dispatcher.Report
	err    error
}

// dispatchWithChildHandoff owns the invocation and its observer together. An
// accepted request stops the pod before any handoff can leave this activity.
func (a *Activities) dispatchWithChildHandoff(ctx context.Context, attempt dispatcher.Attempt, input DispatchStageInput, authority *childStageAuthority) (dispatcher.Report, *journal.ChildHandoffRequest, error) {
	if authority == nil {
		report, err := a.dispatchOrAdopt(ctx, attempt, input)
		return report, nil, err
	}
	observer, ok := a.ChildAuthority.(ChildHandoffObserver)
	if !ok {
		return dispatcher.Report{}, nil, childHandoffFailure(ErrNotConfigured)
	}
	owned, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan childDispatchOutcome, 1)
	go func() {
		report, err := a.dispatchOrAdopt(owned, attempt, input)
		finished <- childDispatchOutcome{report, err}
	}()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var output childDispatchOutcome
	for {
		select {
		case output = <-finished:
			// Shutdown hands the same live pod to another worker; it must keep the
			// grant and resume observation there, not close this attempt.
			if errors.Is(output.err, dispatcher.ErrWorkerHandoff) {
				return output.report, nil, output.err
			}
			final, closeFinal := context.WithTimeout(context.WithoutCancel(ctx), childAuthorityRelease)
			request := authority.observation
			request.Finish = true
			handoff, err := observer.ObserveChildHandoff(final, request)
			closeFinal()
			if err != nil {
				return output.report, nil, childHandoffFailure(err)
			}
			if handoff == nil {
				return output.report, nil, output.err
			}
			return checkedChildHandoff(input, output, handoff)
		case <-ticker.C:
			handoff, err := observer.ObserveChildHandoff(owned, authority.observation)
			if err == nil && handoff == nil {
				continue
			}
			if errors.Is(err, ErrChildCustodyUnavailable) && owned.Err() == nil {
				continue
			}
			cancel()
			output = <-finished
			if errors.Is(output.err, dispatcher.ErrWorkerHandoff) {
				return output.report, nil, output.err
			}
			if err != nil {
				return output.report, nil, childHandoffFailure(err)
			}
			// Revoke and re-read after stop. The final read must still name the same
			// accepted custody; a replaced or cancelled grant cannot authorize it.
			final, closeFinal := context.WithTimeout(context.WithoutCancel(ctx), childAuthorityRelease)
			request := authority.observation
			request.Finish = true
			confirmed, confirmErr := observer.ObserveChildHandoff(final, request)
			closeFinal()
			if confirmErr != nil || confirmed == nil || *confirmed != *handoff {
				return output.report, nil, childHandoffFailure(errors.Join(errors.New("child acceptance changed during stop"), confirmErr))
			}
			return checkedChildHandoff(input, output, confirmed)
		}
	}
}

func checkedChildHandoff(input DispatchStageInput, output childDispatchOutcome, handoff *journal.ChildHandoffRequest) (dispatcher.Report, *journal.ChildHandoffRequest, error) {
	if !input.ChildWorkflowsEnabled || handoff.Validate(input.Envelope.ChildWorkflowOrigin) != nil || handoff.Gaggle != input.Envelope.Gaggle || handoff.ParentRunID != input.Envelope.RunID || handoff.Action != "wait" || handoff.RequestID != ChildHandoffDigest(*handoff) {
		return output.report, nil, childHandoffFailure(errors.New("child handoff differs from lent authority"))
	}
	if !output.report.ParentWritersStopped {
		return output.report, nil, childHandoffFailure(errors.New("parent pod writers are not confirmed stopped"))
	}
	return output.report, handoff, nil
}

func childHandoffSettled(handoff *journal.ChildHandoffRequest, err error) bool {
	return handoff != nil || isChildHandoffFailure(err)
}
