package childworkflow

import (
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ValidateRetainedCustody checks an immutable accepted envelope and source,
// independently of current permission to execute that source. Callers first
// verify the queue's original payload/actor digest and live occurrence owner.
func ValidateRetainedCustody(a Authority, e ChildStartEnvelope, child triggerqueue.ChildRecord, source triggerqueue.ChildProposal) error {
	if e.Identity() != child.Identity || e.Gaggle != a.Origin.Gaggle || e.ParentRunID != a.Origin.RunID || e.StageOccurrence != a.Origin.StageOccurrence || e.ParentStage != a.Admission.ParentTask.Name || e.ConfigGeneration != a.ConfigGeneration || e.ParentWorkflow != a.ParentWorkflow || e.ParentWorkflowDigest != a.ParentWorkflowDigest || e.ParentGooberDigest != a.ParentGooberDigest {
		return ErrAuthorityUnavailable
	}
	if e.SourceDigest != child.ProposalDigest || e.SourceDigest != source.Digest || e.SourceDigest != digest(source.Source) {
		return ErrSubmissionInvalid
	}
	return nil
}
