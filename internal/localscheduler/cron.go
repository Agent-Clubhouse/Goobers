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
func InLocation(s Schedule, loc *time.Location) Schedule {
	return locatedSchedule{s: s, loc: loc}
}

type locatedSchedule struct {
	s   Schedule
	loc *time.Location
}

func (l locatedSchedule) Next(after time.Time) time.Time {
	return l.s.Next(after.In(l.loc))
}
