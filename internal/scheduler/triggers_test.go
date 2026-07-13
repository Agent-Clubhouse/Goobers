package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

// fakeBacklog is a minimal providers.BacklogProvider for tests.
type fakeBacklog struct {
	items   []providers.WorkItem
	listReq providers.ListWorkItemsRequest
	updated []providers.UpdateWorkItemStatusRequest
	err     error
}

func (f *fakeBacklog) ListWorkItems(_ context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	f.listReq = req
	return f.items, f.err
}

func (f *fakeBacklog) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}

func (f *fakeBacklog) CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}

func (f *fakeBacklog) UpdateWorkItemStatus(_ context.Context, req providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error) {
	f.updated = append(f.updated, req)
	return providers.WorkItem{ID: req.ID, Status: req.Status}, f.err
}

func (f *fakeBacklog) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return nil, f.err
}

func (f *fakeBacklog) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{ID: req.ID}, f.err
}

func (f *fakeBacklog) ClaimWorkItem(_ context.Context, req providers.ClaimWorkItemRequest) (providers.ClaimResult, error) {
	return providers.ClaimResult{Claimed: true, ClaimedBy: req.RunID, Item: providers.WorkItem{ID: req.ID}}, f.err
}

func TestScheduleTriggerEmitsOnTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	out := make(chan Event, 1)
	tr := ScheduleTrigger{WorkflowName: "flow", Ticks: ticks, KeyFor: func(time.Time) string { return "tick-1" }}
	go func() { _ = tr.Watch(ctx, out) }()

	ticks <- time.Unix(0, 0)
	ev := <-out
	if ev.WorkflowName != "flow" || ev.Reason != "schedule" || ev.DedupeKey != "tick-1" {
		t.Errorf("unexpected event: %+v", ev)
	}
	if tr.Name() != "schedule:flow" {
		t.Errorf("name = %q", tr.Name())
	}
}

func TestBacklogPollTriggerEmitsPerItem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fb := &fakeBacklog{items: []providers.WorkItem{
		{Provider: providers.ProviderGitHub, ID: "1"},
		{Provider: providers.ProviderGitHub, ID: "2"},
	}}
	ticks := make(chan time.Time, 1)
	out := make(chan Event, 4)
	tr := BacklogPollTrigger{
		WorkflowName: "flow",
		Provider:     fb,
		Repo:         providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		Labels:       []string{"goobers"},
		Ticks:        ticks,
		Limit:        10,
	}
	go func() { _ = tr.Watch(ctx, out) }()

	ticks <- time.Unix(0, 0)
	ev1 := <-out
	ev2 := <-out
	if ev1.DedupeKey != "github:1" || ev2.DedupeKey != "github:2" {
		t.Errorf("dedupe keys = %q,%q; want github:1,github:2", ev1.DedupeKey, ev2.DedupeKey)
	}
	if ev1.Item == nil || ev1.Item.ID != "1" {
		t.Errorf("event missing item: %+v", ev1.Item)
	}
	if len(fb.listReq.Labels) != 1 || fb.listReq.Labels[0] != "goobers" {
		t.Errorf("poll did not pass the label selector: %+v", fb.listReq.Labels)
	}
}

func TestEventTriggerForwardsItems(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan providers.WorkItemEvent, 1)
	out := make(chan Event, 1)
	tr := EventTrigger{WorkflowName: "flow", Events: events}
	go func() { _ = tr.Watch(ctx, out) }()

	events <- providers.WorkItemEvent{
		Provider: providers.ProviderGitHub,
		Item:     providers.WorkItem{Provider: providers.ProviderGitHub, ID: "7"},
		Action:   "available",
	}
	ev := <-out
	if ev.Item == nil || ev.Item.ID != "7" || ev.DedupeKey != "github:7" || ev.Reason != "event:available" {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestTriggerWatchStopsOnClosedSource(t *testing.T) {
	ticks := make(chan time.Time)
	close(ticks)
	out := make(chan Event, 1)
	if err := (ScheduleTrigger{WorkflowName: "flow", Ticks: ticks}).Watch(context.Background(), out); err != nil {
		t.Errorf("closed source should end Watch cleanly, got: %v", err)
	}
}
