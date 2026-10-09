package childpublication

import (
	"context"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// validateTarget checks the original accepted execution. The host separately
// verifies retained source and holds current publication authority across effects.
func (p Publisher) validateTarget(ctx context.Context, t Target) (triggerqueue.ChildRecord, error) {
	if err := t.validate(); err != nil {
		return triggerqueue.ChildRecord{}, err
	}
	c, err := p.Queue.GetChild(ctx, t.Child.Identity)
	if err != nil {
		return triggerqueue.ChildRecord{}, err
	}
	if c.RunID != t.Identity.RunID || c.AcceptanceID != t.Child.AcceptanceID || c.CancellationRequested || c.State.Terminal() || !c.AcknowledgedAt.IsZero() || !c.TombstonedAt.IsZero() {
		return triggerqueue.ChildRecord{}, triggerqueue.ErrTransition
	}
	return c, nil
}
