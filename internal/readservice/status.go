package readservice

import (
	"context"
	"fmt"
	"sort"
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
	DaemonRestart         *DaemonRestartStatus
}

// DaemonRestartStatus correlates the latest daemon lifetime with runs selected
// for automatic recovery during that startup.
type DaemonRestartStatus struct {
	At           time.Time        `json:"at"`
	Reason       string           `json:"reason"`
	PID          int              `json:"pid,omitempty"`
	Version      string           `json:"version,omitempty"`
	Root         string           `json:"root,omitempty"`
	RunIDs       []string         `json:"runIds"`
	Replacements []RunReplacement `json:"replacements,omitempty"`
}

// RunReplacement identifies a failed pre-restart run and the post-restart run
// that claimed the same backlog item.
type RunReplacement struct {
	ItemID           string `json:"itemId"`
	FailedRunID      string `json:"failedRunId"`
	ReplacementRunID string `json:"replacementRunId"`
}

// ListStatusRuns returns every readable run in display order. Individual
// malformed historical journals are omitted so status remains best-effort.
func (s *Local) ListStatusRuns(ctx context.Context) ([]RunSummary, error) {
	return s.runSummaries(ctx, true)
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
	var restart *DaemonRestartStatus
	var sawDaemonStart bool
	var dirtyReason string
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return SchedulerStatus{}, err
		}
		switch event.Type {
		case journal.EventTickSkipped:
			if candidate, ok := parseProviderQuotaResumeTime(event.Reason); ok {
				candidate = candidate.UTC()
				resetAt = &candidate
			}
		case journal.EventDaemonDirtyRestart:
			dirtyReason = event.Reason
		case journal.EventDaemonStarted:
			if sawDaemonStart || dirtyReason != "" {
				reason := "clean restart"
				if dirtyReason != "" {
					reason = dirtyReason
				}
				restart = &DaemonRestartStatus{
					At:      event.Time,
					Reason:  reason,
					PID:     runnerInt(event.Runner, "pid"),
					Version: runnerString(event.Runner, "version"),
					Root:    runnerString(event.Runner, "instanceRoot"),
				}
			}
			sawDaemonStart = true
			dirtyReason = ""
		case journal.EventRunnerAnnotation:
			if restart != nil &&
				runnerString(event.Runner, "kind") == journal.RunnerAnnotationRunRecovery &&
				event.RunID != "" &&
				!containsString(restart.RunIDs, event.RunID) {
				restart.RunIDs = append(restart.RunIDs, event.RunID)
			}
		}
	}
	if restart != nil {
		restart.Replacements, err = s.restartReplacements(ctx, restart.At)
		if err != nil {
			return SchedulerStatus{}, err
		}
	}
	return SchedulerStatus{ProviderQuotaResumeAt: resetAt, DaemonRestart: restart}, nil
}

type restartRun struct {
	id         string
	itemID     string
	startedAt  time.Time
	failedAt   time.Time
	isReplaced bool
}

func (s *Local) restartReplacements(ctx context.Context, restartedAt time.Time) ([]RunReplacement, error) {
	runIDs, err := s.RunIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list runs for restart replacements: %w", err)
	}
	var failed, replacements []restartRun
	for _, runID := range runIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, err := s.openRun(runID)
		if err != nil {
			continue
		}
		if run.identity.Trigger.Kind != journal.TriggerItem || run.identity.Trigger.Ref == "" {
			continue
		}
		candidate := restartRun{
			id:        runID,
			itemID:    run.identity.Trigger.Ref,
			startedAt: run.identity.StartedAt,
		}
		for _, record := range run.records {
			event := record.Event
			if event.Type == journal.EventRunFinished &&
				event.Status == string(journal.PhaseFailed) &&
				!event.Time.Before(restartedAt) {
				candidate.failedAt = event.Time
			}
		}
		if !candidate.failedAt.IsZero() && candidate.startedAt.Before(restartedAt) {
			failed = append(failed, candidate)
		}
		if !candidate.startedAt.Before(restartedAt) {
			replacements = append(replacements, candidate)
		}
	}
	sort.Slice(replacements, func(i, j int) bool {
		return replacements[i].startedAt.Before(replacements[j].startedAt)
	})
	var result []RunReplacement
	for _, old := range failed {
		for i := range replacements {
			replacement := &replacements[i]
			if replacement.isReplaced ||
				replacement.itemID != old.itemID ||
				replacement.startedAt.Before(old.failedAt) {
				continue
			}
			result = append(result, RunReplacement{
				ItemID:           old.itemID,
				FailedRunID:      old.id,
				ReplacementRunID: replacement.id,
			})
			replacement.isReplaced = true
			break
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ItemID == result[j].ItemID {
			return result[i].FailedRunID < result[j].FailedRunID
		}
		return result[i].ItemID < result[j].ItemID
	})
	return result, nil
}

func runnerString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func runnerInt(values map[string]any, key string) int {
	value, _ := values[key].(float64)
	return int(value)
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
