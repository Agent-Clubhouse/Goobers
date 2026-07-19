package readservice

import (
	"context"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

const providerQuotaResumePrefix = localscheduler.ReasonProviderQuota + ": resumes at "

// SchedulerStatus is scheduler state projected from the instance journal for
// local status adapters.
type SchedulerStatus struct {
	ProviderQuotaResumeAt *time.Time
}

// SchedulerStatus returns the current scheduler status recorded in the
// instance journal.
func (s *Local) SchedulerStatus(ctx context.Context) (SchedulerStatus, error) {
	if err := ctx.Err(); err != nil {
		return SchedulerStatus{}, err
	}
	events, err := journal.ReadInstanceLog(s.sources.Layout.SchedulerDir())
	if err != nil {
		return SchedulerStatus{}, err
	}
	var resetAt *time.Time
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return SchedulerStatus{}, err
		}
		if event.Type != journal.EventTickSkipped {
			continue
		}
		if candidate, ok := parseProviderQuotaResumeTime(event.Reason); ok {
			candidate = candidate.UTC()
			resetAt = &candidate
		}
	}
	return SchedulerStatus{ProviderQuotaResumeAt: resetAt}, nil
}

func parseProviderQuotaResumeTime(reason string) (time.Time, bool) {
	if !strings.HasPrefix(reason, providerQuotaResumePrefix) {
		return time.Time{}, false
	}
	resetAt, err := time.Parse(time.RFC3339, strings.TrimPrefix(reason, providerQuotaResumePrefix))
	if err != nil {
		return time.Time{}, false
	}
	return resetAt, true
}
