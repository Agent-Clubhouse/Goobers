package main

import (
	"context"

	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (u *upSession) readChildHistory(ctx context.Context, parent triggerqueue.ChildParent, after string, limit int) ([]triggerqueue.ChildRecord, error) {
	if u.durableTriggers == nil {
		return nil, readservice.ErrChildHistoryUnavailable
	}
	return u.durableTriggers.queue.Children(ctx, parent, after, limit)
}

func (u *upSession) readChildPublications(ctx context.Context, child triggerqueue.ChildIdentity) ([]readservice.ChildPublicationObservation, error) {
	if u.durableTriggers == nil {
		return nil, readservice.ErrChildHistoryUnavailable
	}
	statuses, err := childpublication.Inspect(ctx, u.durableTriggers.queue, child)
	if err != nil {
		return nil, err
	}
	out := make([]readservice.ChildPublicationObservation, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, readservice.ChildPublicationObservation{SourceRunID: status.SourceRunID, Item: readservice.ChildPublicationItem{Action: string(status.Action), State: status.State, Head: status.Head, Base: status.Base, Commit: status.Commit, PullRequestURL: status.PullRequestURL, PullRequestNumber: status.PullRequestNumber, NeedsHuman: status.NeedsHuman}})
	}
	return out, nil
}
