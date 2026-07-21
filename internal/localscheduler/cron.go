package localscheduler

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron"
)

// Schedule is a parsed cron/interval expression: Next reports the next fire
// time strictly after the given instant, in that instant's location — the
// standard Go idiom for DST-correct calendar arithmetic (adding/subtracting
// calendar fields, not wall-clock durations, near a transition).
type Schedule interface {
	Next(after time.Time) time.Time
}

// ParseSchedule parses a workflow's declared schedule expression: standard
// 5-field cron, the named descriptors (@hourly, @daily, ...), or "@every
// <duration>" — V0's firing grammar (SCH-041). internal/workflow.CheckSchedules
// structurally validates a looser grammar at compile time (also accepting
// 6-field cron with a seconds column); a 6-field expression passes that
// structural gate but is rejected here, since V0 owns firing on 5-field cron
// only. Fail closed with an actionable error rather than silently
// misinterpreting a seconds column as something else.
func ParseSchedule(expr string) (Schedule, error) {
	expr = strings.TrimSpace(expr)
	if fields := strings.Fields(expr); len(fields) == 6 {
		return nil, fmt.Errorf("localscheduler: 6-field cron (with seconds) is not supported in V0 — %q; use standard 5-field cron, a descriptor, or \"@every <duration>\"", expr)
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("localscheduler: invalid schedule %q: %w", expr, err)
	}
	return sched, nil
}

// InLocation returns a Schedule whose Next always computes in loc, regardless
// of the location the caller's `after` time carries. This is how the
// instance-configured timezone (default local; SCH/§7) is applied uniformly,
// independent of what location a caller's clock happens to hand in.
//
// It also guards against the DST fall-back double-fire (issue #137):
// robfig/cron's Next walks forward via wall-clock field matching (Go time.Date
// reconstruction), which has no notion of "this civil slot already fired" —
// on the day clocks fall back, a daily schedule's time-of-day occurs twice in
// real time (e.g. 1:30 EDT then 1:30 EST, one real hour apart), and Next
// legitimately returns the second occurrence as "strictly after" the first,
// since they're different absolute instants. See Next's own doc comment for
// the detection and skip technique.
func InLocation(s Schedule, loc *time.Location) Schedule {
	return locatedSchedule{s: s, loc: loc}
}

// NextScheduledFire returns the earliest next fire across schedules.
func NextScheduledFire(schedules []Schedule, after time.Time) (time.Time, bool) {
	var earliest time.Time
	for _, schedule := range schedules {
		next := schedule.Next(after)
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
		}
	}
	return earliest, !earliest.IsZero()
}

type locatedSchedule struct {
	s   Schedule
	loc *time.Location
}

// Next computes the underlying schedule's next fire in loc, then checks
// whether that candidate is the DST fall-back repeat of after's own
// wall-clock reading — if the two render to the identical calendar minute in
// loc despite being different absolute instants, that similarity can only
// arise from the repeated hour a fall-back transition creates (Next's
// contract guarantees a strictly-later instant, so an identical civil
// reading is never a legitimate coincidence — it's cron's minute-granularity
// field match landing on the same wall-clock minute a second real time,
// as robfig/cron's algorithm is documented to do). When that happens, this
// advances past it by asking the schedule for the next fire strictly after
// the repeat, so a schedule fires at most once per nominal civil slot even
// across a fall-back transition — matching systemd's OnCalendar= convention
// for a single-valued time component.
func (l locatedSchedule) Next(after time.Time) time.Time {
	after = after.In(l.loc)
	next := l.s.Next(after)
	if sameCivilMinute(after, next) {
		next = l.s.Next(next.In(l.loc))
	}
	return next
}

// sameCivilMinute reports whether a and b render to the identical calendar
// date and hour:minute in their own location — comparing only down to
// minute granularity since that's cron's own field precision; comparing
// seconds/nanoseconds too would make the check brittle against a caller's
// jitter (e.g. Tick's `now` isn't reconstructed at exactly :00 seconds the
// way a fresh cron computation is).
func sameCivilMinute(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd && a.Hour() == b.Hour() && a.Minute() == b.Minute()
}
