package readservice

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
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

// WorkItemLookup reads the current provider state for a claimed item.
type WorkItemLookup func(context.Context, string, string) (providers.WorkItem, error)

// ListStatusRuns returns every readable run in display order. Individual
// malformed historical journals are omitted so status remains best-effort.
func (s *Local) ListStatusRuns(ctx context.Context) ([]RunSummary, error) {
	return s.runSummaries(ctx, true)
}

func (s *Local) decorateOperatorClaims(ctx context.Context, runs []RunSummary, now time.Time) error {
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(s.sources.Layout.SchedulerDir(), "claims.json"))
	if err != nil {
		return fmt.Errorf("read claim leases for status: %w", err)
	}
	for i := range runs {
		if runs[i].Phase == journal.PhaseRunning &&
			runs[i].Operator.HeartbeatAgeMillis != nil {
			runs[i].Operator.Liveness = "recent"
			if s.sources.LivenessTimeout > 0 &&
				*runs[i].Operator.HeartbeatAgeMillis > s.sources.LivenessTimeout.Milliseconds() {
				runs[i].Operator.Liveness = "stale"
				runs[i].Operator.PotentialBlockers = append(
					runs[i].Operator.PotentialBlockers,
					"stage heartbeat is stale",
				)
			}
		}
		active := ledger.ForRunAll(runs[i].ID)
		history := ledger.HistoryForRun(runs[i].ID)
		switch {
		case len(active) > 0:
			entry := active[0]
			expires := entry.ExpiresAt
			runs[i].Operator.Claim.ExpiresAt = &expires
			if entry.ExpiresAt.After(now) {
				runs[i].Operator.Claim.LeaseStatus = "active"
			} else {
				runs[i].Operator.Claim.LeaseStatus = "expired"
			}
			if runs[i].Operator.Issue == nil {
				runs[i].Operator.Issue = &OperatorIssue{Number: entry.ItemID}
			}
		case len(history) > 0:
			runs[i].Operator.Claim.LeaseStatus = "released"
		}
		markerPresent := runs[i].Operator.Claim.ProviderMarker == "recorded"
		if s.sources.WorkItemLookup != nil && runs[i].Operator.Issue != nil &&
			runs[i].Operator.Issue.Number != "" {
			item, err := s.sources.WorkItemLookup(ctx, runs[i].Gaggle, runs[i].Operator.Issue.Number)
			if err != nil {
				return fmt.Errorf("verify provider claim marker for run %q: %w", runs[i].ID, err)
			}
			markerPresent = item.HasLabel(providers.LabelClaimed)
			if runs[i].Operator.Issue.Title == "" {
				runs[i].Operator.Issue.Title = item.Title
			}
		}
		if runs[i].Phase == journal.PhaseRunning &&
			runs[i].Operator.Claim.LeaseStatus != "active" &&
			markerPresent {
			runs[i].Operator.Claim.ProviderMarker = "drift"
			runs[i].Operator.PotentialBlockers = append(
				runs[i].Operator.PotentialBlockers,
				"provider claim marker exists without an active lease",
			)
		} else if runs[i].Operator.Claim.LeaseStatus == "active" && !markerPresent {
			runs[i].Operator.Claim.ProviderMarker = "drift"
			runs[i].Operator.PotentialBlockers = append(
				runs[i].Operator.PotentialBlockers,
				"active claim lease has no recorded provider marker",
			)
		} else if runs[i].Operator.Claim.LeaseStatus == "active" && markerPresent {
			runs[i].Operator.Claim.ProviderMarker = "verified"
		} else if !markerPresent {
			runs[i].Operator.Claim.ProviderMarker = "not-present"
		}
	}
	return nil
}

// TimeToFirstPR merges the retained lifetime milestone with the successful-init
// instance event and every live ref.touched event. Scanning all retained
// journals keeps the metric fail-closed on incomplete history while the
// milestone survives retention.
func (s *Local) TimeToFirstPR(ctx context.Context) (telemetry.TimeToFirstPRMetric, error) {
	var initCompletedAt, firstPROpenAt time.Time
	if s.sources.Telemetry != nil {
		persisted, err := s.sources.Telemetry.TimeToFirstPR(ctx)
		if err != nil {
			return telemetry.TimeToFirstPRMetric{}, err
		}
		if persisted.InitCompletedAt != nil {
			initCompletedAt = *persisted.InitCompletedAt
		}
		if persisted.FirstPROpenAt != nil {
			firstPROpenAt = *persisted.FirstPROpenAt
		}
	}
	instanceEvents, err := journal.ReadInstanceLog(s.sources.Layout.SchedulerDir())
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, fmt.Errorf("read instance journal for time to first PR: %w", err)
	}
	for _, event := range instanceEvents {
		if err := ctx.Err(); err != nil {
			return telemetry.TimeToFirstPRMetric{}, err
		}
		if event.Type == journal.EventInitCompleted && !event.Time.IsZero() &&
			(initCompletedAt.IsZero() || event.Time.Before(initCompletedAt)) {
			initCompletedAt = event.Time
		}
	}
	if initCompletedAt.IsZero() ||
		(!firstPROpenAt.IsZero() && firstPROpenAt.Before(initCompletedAt)) {
		firstPROpenAt = time.Time{}
	}
	runIDs, err := s.RunIDs(ctx)
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, err
	}
	for _, runID := range runIDs {
		if err := ctx.Err(); err != nil {
			return telemetry.TimeToFirstPRMetric{}, err
		}
		run, err := s.openRun(runID)
		if err != nil {
			return telemetry.TimeToFirstPRMetric{}, fmt.Errorf(
				"read run %q for time to first PR: %w",
				runID,
				err,
			)
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
			if initCompletedAt.IsZero() || event.Time.Before(initCompletedAt) {
				continue
			}
			if firstPROpenAt.IsZero() || event.Time.Before(firstPROpenAt) {
				firstPROpenAt = event.Time
			}
		}
	}
	return telemetry.NewTimeToFirstPRMetric(initCompletedAt, firstPROpenAt), nil
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
