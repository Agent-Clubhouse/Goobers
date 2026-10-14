package startintent

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func entryScheduleRevision(entry localscheduler.WorkflowEntry) (string, error) {
	revision, err := localscheduler.ScheduleRevision(entry.Schedules)
	if err == nil && entry.ScheduleCatchUpAll {
		revision = sourceHash("schedule-all", revision)
	}
	return revision, err
}

// ScheduleOccurrences adopts legacy custody before initializing per-declaration positions.
func (s *Sources) ScheduleOccurrences(ctx context.Context, entry localscheduler.WorkflowEntry, initial time.Time, legacy bool, observed time.Time) ([]time.Time, bool, error) {
	if _, _, err := s.ScheduleCursor(ctx, entry, initial, legacy, observed); err != nil {
		return nil, false, err
	}
	revision, err := entryScheduleRevision(entry)
	if err != nil {
		return nil, false, err
	}
	return s.Queue.SourceOccurrences(ctx, scheduleScope(entry), revision, len(entry.Schedules))
}

// AcceptOccurrence pins exactly one nominal occurrence and moves only its declaration.
func (s *Sources) AcceptOccurrence(ctx context.Context, entry localscheduler.WorkflowEntry, index int, before, after, observed time.Time) error {
	if !entry.ScheduleCatchUpAll || index < 0 || index >= len(entry.Schedules) || after.After(observed) || !after.After(before) || !entry.Schedules[index].Next(before).Equal(after) {
		return errors.New("startintent: invalid nominal occurrence")
	}
	revision, err := entryScheduleRevision(entry)
	if err != nil {
		return err
	}
	scope := scheduleScope(entry)
	key := sourceHash("schedule-occurrence", scope, revision, strconv.Itoa(index), before.UTC().Format(time.RFC3339Nano), after.UTC().Format(time.RFC3339Nano))
	source := localscheduler.SourceTrigger{ScheduleDeclaration: index + 1, ScheduledFrom: before.UTC(), ScheduledAt: after.UTC()}
	raw, release, err := s.pin(ctx, entry, source)
	if err != nil {
		return err
	}
	defer release()
	advance := triggerqueue.SourceAdvance{Scope: scope, Revision: revision, Occurrence: index + 1, Before: before, After: after}
	if entry.ScheduleDemandCounter != nil {
		return s.Queue.CaptureScheduleDemand(ctx, triggerqueue.ScheduleDemandCapture{ID: key, Advance: advance, Payload: raw, Independent: true, AcceptedAt: observed})
	}
	_, _, err = s.Queue.AcceptSource(ctx, triggerqueue.SourceBatch{Key: key, Actor: "scheduler", Fingerprint: key, Starts: []triggerqueue.SourceStart{{Payload: raw}}, Advance: &advance}, observed)
	return err
}

// ObserveScheduleAbsence fences future discovery when all current declarations
// disappear. Already accepted starts and demand keep their independent custody.
func (s *Sources) ObserveScheduleAbsence(ctx context.Context, entry localscheduler.WorkflowEntry, observed time.Time) error {
	if len(entry.Schedules) != 0 {
		return errors.New("startintent: schedule absence has active declarations")
	}
	revision, err := entryScheduleRevision(entry)
	if err != nil {
		return err
	}
	return s.Queue.ResetExistingSourceRevision(ctx, scheduleScope(entry), revision, observed)
}
