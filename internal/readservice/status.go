package readservice

import (
	"context"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry"
)

const providerQuotaResumePrefix = localscheduler.ReasonProviderQuota + ": resumes at "

// StatusReader is the shared read boundary used by local status adapters.
type StatusReader interface {
	ListStatusRuns(context.Context) ([]RunSummary, error)
	TimeToFirstPR(context.Context) (telemetry.TimeToFirstPRMetric, error)
	SchedulerStatus(context.Context) (SchedulerStatus, error)
}

// SchedulerStatus is scheduler state projected from the instance journal for
// local status adapters.
type SchedulerStatus struct {
	ProviderQuotaResumeAt *time.Time
}

// ListStatusRuns returns every readable run in display order. Individual
// malformed historical journals are omitted so status remains best-effort.
func (s *Local) ListStatusRuns(ctx context.Context) ([]RunSummary, error) {
	return s.runSummaries(ctx, true)
}

// TimeToFirstPR computes the lifetime onboarding metric directly from the
// immutable run identities and ref.touched events in the run journals.
func (s *Local) TimeToFirstPR(ctx context.Context) (telemetry.TimeToFirstPRMetric, error) {
	runIDs, err := s.RunIDs(ctx)
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, err
	}
	var firstRunAt, firstPROpenAt time.Time
	for _, runID := range runIDs {
		if err := ctx.Err(); err != nil {
			return telemetry.TimeToFirstPRMetric{}, err
		}
		run, err := s.openRun(runID)
		if err != nil {
			continue
		}
		if startedAt := run.identity.StartedAt; !startedAt.IsZero() &&
			(firstRunAt.IsZero() || startedAt.Before(firstRunAt)) {
			firstRunAt = startedAt
		}
		for _, record := range run.records {
			event := record.Event
			operation, _ := event.Runner["operation"].(string)
			if event.Type != journal.EventRefTouched ||
				event.ExternalRef == nil ||
				event.ExternalRef.Kind != "pr" ||
				operation != "open" ||
				event.Time.IsZero() {
				continue
			}
			if firstPROpenAt.IsZero() || event.Time.Before(firstPROpenAt) {
				firstPROpenAt = event.Time
			}
		}
	}
	return telemetry.NewTimeToFirstPRMetric(firstRunAt, firstPROpenAt), nil
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
