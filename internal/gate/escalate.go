package gate

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/providers"
)

// StatusCommenter is the minimal provider seam EscalationNotifier needs.
// providers.BacklogProvider satisfies it directly via UpdateWorkItemStatus.
type StatusCommenter interface {
	UpdateWorkItemStatus(ctx context.Context, req providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error)
}

// EscalationNotifier surfaces a run's escalation to whoever is watching the
// driving backlog item — issue #20's "Escalate target behavior at V0 ...
// surfaced via ... a provider comment on the driving issue/PR if one exists."
// CLI status surfacing (`goobers status`) is the local runner's (#17) job;
// this covers the provider-comment half.
type EscalationNotifier struct {
	Poster     StatusCommenter
	Repository providers.RepositoryRef
	// CurrentStatus is echoed back unchanged: NotifyEscalated posts a comment,
	// it does not advance the item's processing status. The caller must pass
	// the item's actual current status — an empty value strips its status
	// label instead of preserving it (see providers.replaceStatusLabel).
	CurrentStatus providers.WorkItemStatus
}

// NotifyEscalated posts a comment on itemID explaining which gate escalated
// the run and why. A nil Poster/Notifier or empty itemID is a no-op: not
// every run has a driving issue/PR (schedule-triggered producer runs, e.g.),
// and callers should feel free to construct a notifier unconditionally and
// let this handle the "no item" case.
func (n *EscalationNotifier) NotifyEscalated(ctx context.Context, itemID string, r Result, reason string) error {
	if n == nil || n.Poster == nil || itemID == "" {
		return nil
	}
	comment := fmt.Sprintf(
		"Goobers run escalated at gate %q after %d repass attempt(s) (last outcome: %q). %s",
		r.Gate, r.Attempt, r.Outcome, reason,
	)
	if _, err := n.Poster.UpdateWorkItemStatus(ctx, providers.UpdateWorkItemStatusRequest{
		Repository: n.Repository,
		ID:         itemID,
		Status:     n.CurrentStatus,
		Comment:    comment,
	}); err != nil {
		return fmt.Errorf("gate: notify escalation on %s#%s: %w", n.Repository.Name, itemID, err)
	}
	return nil
}
