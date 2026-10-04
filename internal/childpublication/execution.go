package childpublication

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// validateTarget binds publication to the full retained execution, not merely
// the accepted child slot. The daemon separately verifies pinned source and
// current delegated and interactive authority before constructing this target.
func (p Publisher) validateTarget(ctx context.Context, t Target) (triggerqueue.ChildExecution, error) {
	if err := t.validate(); err != nil {
		return triggerqueue.ChildExecution{}, err
	}
	c, err := p.Queue.GetChild(ctx, t.Child.Identity)
	if err != nil {
		return triggerqueue.ChildExecution{}, err
	}
	if c.RunID != t.Child.RunID || c.AcceptanceID != t.Child.AcceptanceID || c.ActiveRunID() != t.Identity.RunID || c.ExecutionEpoch != t.Identity.Child.ExecutionEpoch || c.CancellationRequested || c.State.Terminal() || !c.AcknowledgedAt.IsZero() || !c.TombstonedAt.IsZero() {
		return triggerqueue.ChildExecution{}, triggerqueue.ErrTransition
	}
	if err = p.Queue.CheckChildParentOpen(ctx, c.Identity.ChildParent); err != nil {
		return triggerqueue.ChildExecution{}, err
	}
	e, err := p.Queue.ChildExecution(ctx, c.Identity, t.Identity.RunID)
	if err != nil {
		return triggerqueue.ChildExecution{}, err
	}
	if e.Epoch != t.Identity.Child.ExecutionEpoch || (e.Epoch > 0 && (e.SourceRunID != t.Identity.ContinuedFromRunID || e.SourceTerminalSeq != t.Identity.SourceTerminalSeq || e.Actor != t.Identity.Operator || e.Stage != t.Identity.RequestedTarget || e.SourceResultRef != t.Identity.Child.PriorResultRef || e.RequestDigest != t.Identity.Child.RestartDigest)) {
		return triggerqueue.ChildExecution{}, errors.New("child publication execution differs from retained restart")
	}
	return e, nil
}
