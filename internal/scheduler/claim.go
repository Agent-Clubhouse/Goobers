package scheduler

import (
	"context"

	"github.com/goobers/goobers/providers"
)

// Claimer mirrors a claimed backlog item's status to the backlog for humans. The
// authoritative, exactly-once claim is the engine's deterministic RunID; this is
// only the visible mirror.
type Claimer interface {
	Claim(ctx context.Context, item providers.WorkItem) error
}

// BacklogClaimer marks an item as claimed via the providers abstraction. It is
// idempotent: an item already at or past "claimed" is left untouched.
type BacklogClaimer struct {
	Provider providers.BacklogProvider
	Repo     providers.RepositoryRef
}

// Claim sets the item's mirrored status to claimed, unless it already is.
func (c BacklogClaimer) Claim(ctx context.Context, item providers.WorkItem) error {
	if c.Provider == nil {
		return nil
	}
	switch item.Status {
	case providers.WorkItemStatusClaimed, providers.WorkItemStatusInProgress,
		providers.WorkItemStatusDone, providers.WorkItemStatusClosed:
		return nil // already claimed/handled — nothing to mirror
	}
	_, err := c.Provider.UpdateWorkItemStatus(ctx, providers.UpdateWorkItemStatusRequest{
		Repository: c.Repo,
		ID:         item.ID,
		Status:     providers.WorkItemStatusClaimed,
		Comment:    "claimed by the Goobers scheduler",
	})
	return err
}
