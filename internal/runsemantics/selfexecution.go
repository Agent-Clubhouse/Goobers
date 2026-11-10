package runsemantics

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// SelfExecutionDeniedCode is the stable runtime policy refusal classification.
const SelfExecutionDeniedCode = "placement_refused_self_denied"

// SelfExecutionRefusal is operator policy, never a retryable executor failure.
type SelfExecutionRefusal struct{ Stage string }

func (e *SelfExecutionRefusal) Error() string {
	return fmt.Sprintf("%s: stage %q selected self while placement.selfExecution is deny; configure a non-self runner and placement", SelfExecutionDeniedCode, e.Stage)
}

// StageErrorCode implements the shared classified-stage-error seam.
func (e *SelfExecutionRefusal) StageErrorCode() string { return SelfExecutionDeniedCode }

// SelfExecutionBlockedResult lets the normal blocked terminal emit its alarm,
// park the item and release claims without executing another workflow stage.
func SelfExecutionBlockedResult(stage string) apiv1.ResultEnvelope {
	err := &SelfExecutionRefusal{Stage: stage}
	return apiv1.ResultEnvelope{Status: apiv1.ResultBlocked, Summary: err.Error(), Error: journal.ErrorInfoFor(SelfExecutionDeniedCode, err, false)}
}
