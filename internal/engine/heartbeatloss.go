package engine

import (
	"errors"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

const heartbeatLossInfraChange = "heartbeat-loss-infra-v1"

// classifyAttemptFailure is the stage retry loop's class for one failed
// dispatch. A lost heartbeat means the worker or its Temporal path went away
// (#6750); the stage itself reported nothing. For a stage that declares no
// policy actions it is retried on the infrastructure budget like an
// unconfirmed surrender, and the dispatcher retires the prior attempt's pod
// before the retry's pod starts. Side-effecting stages remain refused by the
// caller's worker-loss rule. Older histories keep their recorded policy class.
func classifyAttemptFailure(ctx workflow.Context, t apiv1.Task, err error) (journal.AttemptClass, error) {
	class, cerr := ClassifyDispatchFailure(err)
	if cerr != nil || class != journal.AttemptPolicy || len(t.PolicyActions) > 0 || !isHeartbeatTimeout(err) {
		return class, cerr
	}
	if workflow.GetVersion(ctx, heartbeatLossInfraChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return class, nil
	}
	return journal.AttemptInfra, nil
}

func isHeartbeatTimeout(err error) bool {
	var timeoutErr *temporal.TimeoutError
	return errors.As(err, &timeoutErr) && timeoutErr.TimeoutType() == enumspb.TIMEOUT_TYPE_HEARTBEAT
}

// isWorkerLoss reports an attempt whose worker may have been lost after the
// stage committed an external effect: a start-to-close or heartbeat timeout,
// or an activity whose own control path was lost before it could settle.
func isWorkerLoss(err error) bool {
	var timeoutErr *temporal.TimeoutError
	if errors.As(err, &timeoutErr) {
		return timeoutErr.TimeoutType() == enumspb.TIMEOUT_TYPE_START_TO_CLOSE || timeoutErr.TimeoutType() == enumspb.TIMEOUT_TYPE_HEARTBEAT
	}
	return hasFailureType(err, failureTypeWorkerLost)
}

// hasFailureType reports whether any application error in err's cause chain
// carries failureType.
func hasFailureType(err error, failureType string) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var appErr *temporal.ApplicationError
		if !errors.As(current, &appErr) {
			return false
		}
		if appErr.Type() == failureType {
			return true
		}
		current = appErr
	}
	return false
}

// failureTypePriorAttemptLive marks a retry the dispatcher refused because an
// earlier attempt's writable pod still runs without confirmed custody (#6750).
const failureTypePriorAttemptLive = "GoobersPriorAttemptLive"

// maxPriorAttemptDeferrals bounds fence refusals that do not spend the
// infrastructure budget. One normally suffices: the deferred retry waits
// until the held pod's activeDeadlineSeconds has stopped it.
const maxPriorAttemptDeferrals = 2

// chargeInfraFailure counts one infrastructure-classed failed dispatch. A
// fence deferral reports no fault, only that the attempt it replaces has not
// stopped yet, so within its own bound it extends the dispatch allowance
// instead of spending the infrastructure budget the lost attempt already
// charged. The marker only exists in histories written by this code.
func chargeInfraFailure(err error, infraFailures, deferrals, maxAttempts *int32) {
	if hasFailureType(err, failureTypePriorAttemptLive) && *deferrals < maxPriorAttemptDeferrals {
		*deferrals++
		*maxAttempts++
		return
	}
	*infraFailures++
}
