package localscheduler

import (
	"errors"
	"time"

	"github.com/robfig/cron/v3"
)

const scheduleCatchUpWindow = time.Hour

// ScheduleWindowEvaluation bounds discovery, not custody already accepted by
// the queue. LastEval advances over expired fires only when such fires exist.
type ScheduleWindowEvaluation struct {
	Tick    TickResult
	From    time.Time
	Indexes []int
}

// EvaluateScheduleWindow coalesces only occurrences in the preceding hour.
// Interval schedules retain their previous phase while finding that window;
// checking a two-hour interval every minute must never postpone its next fire.
func EvaluateScheduleWindow(schedules []Schedule, before, now time.Time) (ScheduleWindowEvaluation, error) {
	result := ScheduleWindowEvaluation{Tick: TickResult{LastEval: before}, From: before}
	if !now.After(before) {
		return result, nil
	}
	if floor := now.Add(-scheduleCatchUpWindow); floor.After(result.From) {
		result.From = floor
	}
	expired := false
	for index, schedule := range schedules {
		first := schedule.Next(before)
		if first.IsZero() {
			continue
		}
		if !first.After(before) {
			return result, errors.New("localscheduler: schedule did not advance")
		}
		if !first.After(result.From) {
			expired = true
		}
		next := first
		if !next.After(result.From) {
			next = nextScheduleInWindow(schedule, first, result.From)
		}
		matched := false
		for count := 0; !next.IsZero() && !next.After(now); count++ {
			if count >= 3601 || !next.After(result.From) {
				return result, errors.New("localscheduler: schedule exceeds bounded hourly discovery")
			}
			result.Tick.MissedTicks++
			matched = true
			previous := next
			next = schedule.Next(next)
			if !next.IsZero() && !next.After(previous) {
				return result, errors.New("localscheduler: schedule did not advance")
			}
		}
		if matched {
			result.Indexes = append(result.Indexes, index)
		}
	}
	result.Tick.Fire = result.Tick.MissedTicks > 0
	result.Tick.CatchUp = result.Tick.MissedTicks > 1
	if result.Tick.Fire || expired {
		result.Tick.LastEval = now
	}
	return result, nil
}

func nextScheduleInWindow(schedule Schedule, first, floor time.Time) time.Time {
	if delay := constantScheduleDelay(schedule); delay > 0 {
		// The multiplication is bounded by floor-first. Next also preserves the
		// location wrapper's repeated-civil-time rule at the window boundary.
		previous := first.Add((floor.Sub(first) / delay) * delay)
		return schedule.Next(previous)
	}
	return schedule.Next(floor)
}

func constantScheduleDelay(schedule Schedule) time.Duration {
	switch s := schedule.(type) {
	case authoredSchedule:
		return constantScheduleDelay(s.Schedule)
	case locatedSchedule:
		return constantScheduleDelay(s.s)
	case cron.ConstantDelaySchedule:
		return s.Delay
	default:
		return 0
	}
}

func (source SourceTrigger) validateScheduleWindow() error {
	if source.ScheduleWindowFrom.IsZero() && source.ScheduleFireCount == 0 {
		return nil
	}
	if source.Signal != "" || source.WorkerKind != "" || source.ScheduleFireCount < 1 || source.ScheduleWindowFrom.Before(source.ScheduledFrom) || !source.ScheduledAt.After(source.ScheduleWindowFrom) || source.ScheduledAt.Sub(source.ScheduleWindowFrom) > scheduleCatchUpWindow {
		return errors.New("localscheduler: invalid scheduled catch-up window")
	}
	return nil
}

// scheduleEvaluation checks retained provenance against its archived schedules.
// Legacy receipts retain their original unbounded evaluation semantics.
func (source SourceTrigger) scheduleEvaluation(schedules []Schedule) (ScheduleWindowEvaluation, error) {
	if source.ScheduleWindowFrom.IsZero() {
		return ScheduleWindowEvaluation{
			Tick:    Tick(TriggerState{Schedules: schedules, LastEval: source.ScheduledFrom}, source.ScheduledAt),
			From:    source.ScheduledFrom,
			Indexes: dueScheduleIndexes(schedules, source.ScheduledFrom, source.ScheduledAt),
		}, nil
	}
	window, err := EvaluateScheduleWindow(schedules, source.ScheduledFrom, source.ScheduledAt)
	if err != nil || !window.From.Equal(source.ScheduleWindowFrom) || window.Tick.MissedTicks != source.ScheduleFireCount || !window.Tick.Fire {
		return ScheduleWindowEvaluation{}, errors.New("localscheduler: captured schedule window differs from archived definition")
	}
	return window, nil
}
