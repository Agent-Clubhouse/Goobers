package startintent

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ScheduleCursor reads the durable workflow cursor, seeded once from old history.
func (s *Sources) ScheduleCursor(ctx context.Context, entry localscheduler.WorkflowEntry, initial time.Time, legacy bool) (time.Time, bool, error) {
	return s.Queue.SourceCursorWithLegacy(ctx, scheduleScope(entry), initial, legacy)
}

// AcceptSchedule atomically captures a coalesced firing and advances its cursor.
// An adaptive-backoff skip records an empty batch with the same cursor boundary.
func (s *Sources) AcceptSchedule(ctx context.Context, entry localscheduler.WorkflowEntry, before, after time.Time, fire bool) error {
	scope := scheduleScope(entry)
	key := sourceHash("schedule", scope, before.UTC().Format(time.RFC3339Nano), after.UTC().Format(time.RFC3339Nano))
	fingerprint := sourceHash("schedule", scope, before.UTC().Format(time.RFC3339Nano), after.UTC().Format(time.RFC3339Nano))
	batch := triggerqueue.SourceBatch{Key: key, Actor: "scheduler", Fingerprint: fingerprint, Advance: &triggerqueue.SourceAdvance{Scope: scope, Before: before, After: after}}
	if fire {
		raw, release, err := s.pin(ctx, entry, localscheduler.SourceTrigger{ScheduledFrom: before.UTC(), ScheduledAt: after.UTC()})
		if err != nil {
			return err
		}
		defer release()
		batch.Starts = []triggerqueue.SourceStart{{Payload: raw}}
	}
	_, _, err := s.Queue.AcceptSource(ctx, batch, after)
	return err
}

func scheduleScope(entry localscheduler.WorkflowEntry) string {
	return sourceHash("schedule-cursor", entry.Gaggle, entry.Workflow)
}
