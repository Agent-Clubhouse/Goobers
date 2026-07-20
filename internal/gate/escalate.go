package gate

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/providers"
)

// Commenter is the minimal provider seam EscalationNotifier needs.
// providers.BacklogProvider satisfies it directly via UpdateWorkItem.
// UpdateWorkItem (not UpdateWorkItemStatus) is deliberate: it takes a
// comment-only request with no other field set, so it cannot accidentally
// touch the item's processing-status label — UpdateWorkItemStatus's entire
// purpose is mirroring that label, making it the wrong seam for a pure
// annotation (flagged in #63 QA review, confirmed against #12's provider).
type Commenter interface {
	UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

// EscalationNotifier surfaces a run's escalation to whoever is watching the
// driving backlog item — issue #20's "Escalate target behavior at V0 ...
// surfaced via ... a provider comment on the driving issue/PR if one exists."
// CLI status surfacing (`goobers status`) is the local runner's (#17) job;
// this covers the provider-comment half.
type EscalationNotifier struct {
	Poster     Commenter
	Repository providers.RepositoryRef
}

// NotifyEscalated posts a comment on itemID explaining which gate escalated
// the run and why.
func (n *EscalationNotifier) NotifyEscalated(ctx context.Context, itemID string, r Result, reason string) error {
	comment := fmt.Sprintf(
		"Goobers run escalated at gate %q after %d repass attempt(s) (last outcome: %q). %s",
		r.Gate, r.Attempt, r.Outcome, reason,
	)
	return n.post(ctx, itemID, comment)
}

// NotifyStageEscalated posts a comment on itemID explaining which stage
// directly escalated the run and why.
func (n *EscalationNotifier) NotifyStageEscalated(ctx context.Context, itemID, stage, reason string) error {
	comment := fmt.Sprintf("Goobers run escalated at stage %q. %s", stage, reason)
	return n.post(ctx, itemID, comment)
}

// post is a no-op without a poster or driving item: not every run has one.
func (n *EscalationNotifier) post(ctx context.Context, itemID, comment string) error {
	if n == nil || n.Poster == nil || itemID == "" {
		return nil
	}
	if _, err := n.Poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: n.Repository,
		ID:         itemID,
		Comment:    comment,
	}); err != nil {
		return fmt.Errorf("notify escalation on %s#%s: %w", n.Repository.Name, itemID, err)
	}
	return nil
}
