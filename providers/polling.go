package providers

import (
	"context"
	"time"
)

// subscribeToWorkItems retries list failures on the next interval because the
// subscription contract exposes only an event channel, not an error channel.
func subscribeToWorkItems(
	ctx context.Context,
	sub TriggerSubscription,
	provider ProviderKind,
	state string,
	list func(context.Context, ListWorkItemsRequest) ([]WorkItem, error),
) <-chan WorkItemEvent {
	interval := sub.PollInterval
	if interval <= 0 {
		interval = time.Minute
	}
	events := make(chan WorkItemEvent, 1)
	go func() {
		defer close(events)
		seen := map[string]time.Time{}
		for {
			items, err := list(ctx, ListWorkItemsRequest{
				Repository: sub.Repository,
				State:      state,
				Limit:      100,
			})
			if err == nil {
				for _, item := range items {
					if !shouldEmitWorkItem(seen, item) {
						continue
					}
					select {
					case <-ctx.Done():
						return
					case events <- WorkItemEvent{
						Provider: provider,
						Kind:     TriggerPolling,
						Item:     item,
						Action:   "available",
					}:
					}
				}
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return events
}
