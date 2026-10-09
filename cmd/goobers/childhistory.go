package main

import (
	"context"

	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (u *upSession) readChildHistory(ctx context.Context, parent triggerqueue.ChildParent, after string, limit int) ([]triggerqueue.ChildRecord, error) {
	if u.durableTriggers == nil {
		return nil, readservice.ErrChildHistoryUnavailable
	}
	return u.durableTriggers.queue.Children(ctx, parent, after, limit)
}
