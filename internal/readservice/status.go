package readservice

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
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

// WorkItemLookup reads the current provider state for a claimed item.
type WorkItemLookup func(context.Context, string, string) (providers.WorkItem, error)

// PullRequestLookup reads the current provider state for a run's pull request.
type PullRequestLookup func(context.Context, string, string) (providers.PullRequestSummary, error)

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
		markerVerified := false
		markerPresent := false
		if s.sources.WorkItemLookup != nil &&
			runs[i].Phase == journal.PhaseRunning &&
			runs[i].Operator.Issue != nil &&
			runs[i].Operator.Issue.Number != "" {
			item, err := s.sources.WorkItemLookup(ctx, runs[i].Gaggle, runs[i].Operator.Issue.Number)
			if err != nil {
				// The reader could not verify the marker; the run itself is
				// unaffected. This belongs to the diagnostics-limitations
				// channel, never to the run's blockers (#3346) — a
				// credential-less `goobers status` reported two healthy runs as
				// blocked and nearly triggered an investigation into them.
				runs[i].Operator.Claim.ProviderMarker = "unavailable"
				runs[i].Operator.DiagnosticsLimitations = append(
					runs[i].Operator.DiagnosticsLimitations,
					"provider claim marker verification unavailable: "+err.Error(),
				)
				continue
			}
			markerVerified = true
			markerPresent = item.HasLabel(providers.LabelClaimed)
			if runs[i].Operator.Issue.Title == "" {
				runs[i].Operator.Issue.Title = item.Title
			}
		}
		if markerVerified &&
			runs[i].Operator.Claim.LeaseStatus != "active" &&
			markerPresent {
			runs[i].Operator.Claim.ProviderMarker = "drift"
			runs[i].Operator.PotentialBlockers = append(
				runs[i].Operator.PotentialBlockers,
				"provider claim marker exists without an active lease",
			)
		} else if markerVerified &&
			runs[i].Operator.Claim.LeaseStatus == "active" &&
			!markerPresent {
			runs[i].Operator.Claim.ProviderMarker = "drift"
			runs[i].Operator.PotentialBlockers = append(
				runs[i].Operator.PotentialBlockers,
				"active claim lease has no provider marker",
			)
		} else if markerVerified &&
			runs[i].Operator.Claim.LeaseStatus == "active" &&
			markerPresent {
			runs[i].Operator.Claim.ProviderMarker = "verified"
		} else if markerVerified && !markerPresent {
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
	id                   string
	itemID               string
	startedAt            time.Time
	failedAt             time.Time
	interruptedByRestart bool
	isReplaced           bool
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
		var daemonRecovery, interruptionPending bool
		for _, record := range run.records {
			event := record.Event
			if event.Type == journal.EventRunnerAnnotation &&
				runnerString(event.Runner, "kind") == journal.RunnerAnnotationRunRecovery &&
				runnerString(event.Runner, "reason") == "daemon_restart" &&
				!event.Time.Before(restartedAt) {
				daemonRecovery = true
			}
			if event.Type == journal.EventStageFinished {
				interrupted, _ := event.Runner["interruptedAttempt"].(bool)
				if interrupted && event.Error != nil && event.Error.Code == "interrupted" {
					interruptionPending = daemonRecovery
				} else {
					interruptionPending = false
				}
			}
			if event.Type == journal.EventStageStarted && interruptionPending {
				interruptionPending = false
			}
			if event.Type == journal.EventRunFinished &&
				event.Status == string(journal.PhaseFailed) &&
				!event.Time.Before(restartedAt) {
				candidate.failedAt = event.Time
				candidate.interruptedByRestart = interruptionPending
			}
		}
		if !candidate.failedAt.IsZero() &&
			candidate.startedAt.Before(restartedAt) &&
			candidate.interruptedByRestart {
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
