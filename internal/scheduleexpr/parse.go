// Package scheduleexpr owns the cron grammar shared by workflow validation
// and the local scheduler.
package scheduleexpr

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var (
	// Definitions accept both standard five-field cron and a leading seconds
	// field. The V0 local scheduler deliberately uses runtimeParser below and
	// rejects the latter with its version-specific diagnostic.
	definitionParser = cron.NewParser(
		cron.SecondOptional |
			cron.Minute |
			cron.Hour |
			cron.Dom |
			cron.Month |
			cron.Dow |
			cron.Descriptor,
	)
	runtimeParser = cron.NewParser(
		cron.Minute |
			cron.Hour |
			cron.Dom |
			cron.Month |
			cron.Dow |
			cron.Descriptor,
	)
)

// ParseDefinition parses the schedule grammar admitted by workflow
// definitions. cron/v3 owns field counts, ranges, descriptors, and @every
// duration validation; Goobers adds only the contract that a valid definition
// must eventually fire.
func ParseDefinition(expr string) (cron.Schedule, error) {
	return parse(definitionParser, expr)
}

// ParseRuntime parses the five-field schedule grammar fired by the V0 local
// scheduler.
func ParseRuntime(expr string) (cron.Schedule, error) {
	return parse(runtimeParser, expr)
}

func parse(parser cron.Parser, expr string) (cron.Schedule, error) {
	expr = strings.TrimSpace(expr)
	// Goobers applies the instance timezone outside the parsed schedule. Letting
	// cron's per-expression timezone prefix through would bypass that policy.
	if strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=") {
		return nil, fmt.Errorf("per-expression timezones are not supported; configure the instance timezone")
	}
	schedule, err := parser.Parse(expr)
	if err != nil {
		return nil, err
	}
	if schedule.Next(time.Unix(0, 0)).IsZero() {
		return nil, fmt.Errorf("expression can never fire")
	}
	return schedule, nil
}
