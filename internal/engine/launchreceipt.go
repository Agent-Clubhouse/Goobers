package engine

import (
	"fmt"

	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
)

const remoteLaunchReceiptChange = "remote-launch-receipt-v1"

// Old histories schedule their original payload. An old outstanding activity
// without a binding fails closed at the new dispatcher; completed activities
// still replay unchanged. No binding is reconstructed from pod-authored data.
func (r *runJournal) remoteLaunchBinding(ctx workflow.Context, stage string, review bool) *launchreceipt.Binding {
	kind := journal.EventStageStarted
	if review {
		kind = journal.EventReviewerStarted
	}
	for i := len(r.proj.Ops) - 1; i >= 0; i-- {
		e := r.proj.Ops[i].Event
		if e == nil || e.Type != kind || e.Stage != stage {
			continue
		}
		// Every valid projection op produces one event, including artifacts
		// and spans (projectedEvents/writeProjectedRun use the same ordering).
		seq := uint64(i + 1)
		// Gate each durable start independently: a legacy dispatch keeps its
		// payload, while a later retry/new visit can acquire the new binding.
		changeID := fmt.Sprintf("%s/%s/%d", remoteLaunchReceiptChange, stage, seq)
		if workflow.GetVersion(ctx, changeID, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
			return nil
		}
		if r.live {
			ack, ok := r.startAcks[r.proj.Ops[i].EmitKey]
			if !ok || ack.Seq == 0 || ack.Type != e.Type || ack.Stage != e.Stage || ack.Branch != e.Branch || ack.Attempt != e.Attempt || ack.Class != e.AttemptClass {
				return nil
			}
			seq = ack.Seq
		}
		id := r.proj.Identity
		return &launchreceipt.Binding{RunID: id.RunID, Stage: stage, Branch: e.Branch, StartedSeq: seq,
			AttemptID: journal.StageAttemptID(id.RunID, e.Branch, stage, seq), Number: e.Attempt, Class: e.AttemptClass,
			Review: review, WorkflowDigest: id.WorkflowDigest, GooberDigest: id.GooberDigest}
	}
	return nil
}
