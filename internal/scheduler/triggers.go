package scheduler

import (
	"context"
	"time"

	"github.com/goobers/goobers/providers"
)

// Trigger is a pluggable source of candidate runs. Watch streams Events to out
// until ctx is cancelled or the trigger's source is exhausted. Trigger types are
// kept side-effect-light and injectable (clock/provider/channel) so they unit
// test without real time or network.
type Trigger interface {
	Name() string
	Watch(ctx context.Context, out chan<- Event) error
}

// dedupeKey is the per-item identity used for exactly-once starting.
func dedupeKey(item providers.WorkItem) string {
	return string(item.Provider) + ":" + item.ID
}

// ScheduleTrigger emits an Event each time it receives a tick. The tick source
// is injected (production wraps a time.Ticker; tests send ticks directly), so
// the trigger needs no wall clock of its own.
type ScheduleTrigger struct {
	WorkflowName string
	Ticks        <-chan time.Time
	// KeyFor derives the per-fire dedupe key from a tick. Defaults to the tick in
	// RFC3339Nano, so distinct ticks start distinct runs.
	KeyFor func(t time.Time) string
}

// Name identifies the trigger.
func (s ScheduleTrigger) Name() string { return "schedule:" + s.WorkflowName }

// Watch forwards each tick as a scheduling Event.
func (s ScheduleTrigger) Watch(ctx context.Context, out chan<- Event) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t, ok := <-s.Ticks:
			if !ok {
				return nil
			}
			key := t.UTC().Format(time.RFC3339Nano)
			if s.KeyFor != nil {
				key = s.KeyFor(t)
			}
			if err := send(ctx, out, Event{WorkflowName: s.WorkflowName, Reason: "schedule", DedupeKey: key}); err != nil {
				return err
			}
		}
	}
}

// BacklogPollTrigger lists matching backlog items on each tick and emits one
// Event per item. Label matching is the routing mechanism: only items carrying
// all of Labels are considered for this workflow.
type BacklogPollTrigger struct {
	WorkflowName string
	Provider     providers.BacklogProvider
	Repo         providers.RepositoryRef
	Labels       []string
	Ticks        <-chan time.Time
	Limit        int
}

// Name identifies the trigger.
func (b BacklogPollTrigger) Name() string { return "backlog-poll:" + b.WorkflowName }

// Watch polls the backlog on each tick and emits an Event per open item.
func (b BacklogPollTrigger) Watch(ctx context.Context, out chan<- Event) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-b.Ticks:
			if !ok {
				return nil
			}
			items, err := b.Provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
				Repository: b.Repo,
				Labels:     b.Labels,
				State:      "open",
				Limit:      b.Limit,
			})
			if err != nil {
				return err
			}
			for i := range items {
				item := items[i]
				ev := Event{WorkflowName: b.WorkflowName, Item: &item, Reason: "backlog-item", DedupeKey: dedupeKey(item)}
				if err := send(ctx, out, ev); err != nil {
					return err
				}
			}
		}
	}
}

// EventTrigger forwards a provider's work-item events (webhook/polling
// subscription) as scheduling Events.
type EventTrigger struct {
	WorkflowName string
	Events       <-chan providers.WorkItemEvent
}

// Name identifies the trigger.
func (e EventTrigger) Name() string { return "event:" + e.WorkflowName }

// Watch forwards each provider event as a scheduling Event.
func (e EventTrigger) Watch(ctx context.Context, out chan<- Event) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pe, ok := <-e.Events:
			if !ok {
				return nil
			}
			item := pe.Item
			ev := Event{WorkflowName: e.WorkflowName, Item: &item, Reason: "event:" + pe.Action, DedupeKey: dedupeKey(item)}
			if err := send(ctx, out, ev); err != nil {
				return err
			}
		}
	}
}

func send(ctx context.Context, out chan<- Event, ev Event) error {
	select {
	case out <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
