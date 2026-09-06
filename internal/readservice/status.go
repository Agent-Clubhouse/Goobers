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
	"github.com/goobers/goobers/internal/readmodel"
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
	// RefusedWorkflows are the workflows the startup constraint solve marked
	// unplaceable on the declared runners: inventory (workflow.refused,
	// #2860/dsl-3.0.md §5 checkpoint 3) for the configuration currently in
	// force: the set resets at each daemon start and each accepted config
	// reload, because the scheduler re-journals current refusals at both
	// boundaries. Empty on zero-declaration instances.
	RefusedWorkflows []WorkflowRefusalStatus
	RefillOccupancy  []RefillOccupancyStatus
	Retention        *RetentionStatus
}

// WorkflowRefusalStatus is one boot-refused workflow and its solver
// diagnostic.
type WorkflowRefusalStatus struct {
	Gaggle   string    `json:"gaggle,omitempty"`
	Workflow string    `json:"workflow"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}

// RefillOccupancyStatus summarizes desired occupancy state for one workflow.
type RefillOccupancyStatus struct {
	Gaggle            string `json:"gaggle"`
	Workflow          string `json:"workflow"`
	DesiredRuns       int32  `json:"desiredRuns"`
	ActiveRuns        int32  `json:"activeRuns"`
	AdmissionBlocked  bool   `json:"admissionBlocked"`
	BlockingCondition string `json:"blockingCondition,omitempty"`
}

// RetentionStatus exposes projection retention diagnostics for the portal.
type RetentionStatus struct {
	// Window is the configured retention window in days, or 0 for unbounded.
	Window int `json:"window"`
	// AgedOut is the cumulative number of runs aged out of the projection.
	AgedOut int `json:"agedOut"`
	// Passes is the total number of retention passes executed.
	Passes int `json:"passes"`
	// LastPassAt is the time of the most recent retention pass, if any.
	LastPassAt *time.Time `json:"lastPassAt,omitempty"`
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

// ListStatusRuns returns every readable run in display order. Individual
// malformed historical journals are omitted so status remains best-effort.
//
// `status`'s callers need the FULL run population, not a page of it: the
// fleet summary and the in-flight agent probe both aggregate across every
// run, and only the CLI table itself truncates to --limit (applied locally,
// after this call returns — see cmd/goobers/status.go's selectStatusRuns).
// So the fix for #4249 is not to bound this result — it is to stop sourcing
// it by opening and parsing every run journal (`runSummariesForStage`,
// os.ReadDir + journal.OpenRead per run) when the read-model projection can
// answer the same rows with zero journal opens. Falls back to the journal
// walk only when the projection is unavailable, so an instance without one
// keeps today's behavior unchanged.
func (s *Local) ListStatusRuns(ctx context.Context) ([]RunSummary, error) {
	if s.readModelReads && s.sources.ReadModel != nil {
		return s.listStatusRunsFromReadModel(ctx)
	}
	return s.runSummaries(ctx, true)
}

// listStatusRunsFromReadModel pages through every projected run (§15.9's
// "list/unrestricted" combination — an empty ListOptions is a supported,
// indexed query shape, not a fallback) rather than opening a journal per row.
// The store caps each page at its own maxListLimit regardless of what is
// requested, so this loops on the returned cursor; even a fleet of 11,040
// runs is a few dozen indexed queries, not 11,040 file opens.
func (s *Local) listStatusRunsFromReadModel(ctx context.Context) ([]RunSummary, error) {
	observedAt := s.now()
	var (
		out    []RunSummary
		cursor readmodel.ListCursor
	)
	for {
		page, err := s.sources.ReadModel.ListRuns(ctx, readmodel.ListOptions{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, row := range page.Runs {
			out = append(out, summaryFromReadModel(row, observedAt))
		}
		if !page.HasMore {
			break
		}
		cursor = page.Next
	}
	if err := s.decorateOperatorClaims(ctx, out, observedAt); err != nil {
		return nil, err
	}
	return out, nil
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
	projected, err := s.instanceLog.snapshot(ctx, s.sources.Layout.SchedulerDir())
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, fmt.Errorf("read instance journal for time to first PR: %w", err)
	}
	if journaled := projected.initCompletedAt; !journaled.IsZero() &&
		(initCompletedAt.IsZero() || journaled.Before(initCompletedAt)) {
		initCompletedAt = journaled
	}
	if initCompletedAt.IsZero() ||
		(!firstPROpenAt.IsZero() && firstPROpenAt.Before(initCompletedAt)) {
		firstPROpenAt = time.Time{}
	}
	// #4249: once the persisted rollup already names a firstPROpenAt after
	// init completed, re-deriving it from a full journal walk on every call
	// cannot improve on it — only match it, at O(all runs) cost instead of
	// O(1). upsertTimeToFirstPR (internal/telemetry/rollup/onboarding.go)
	// only ever narrows this value toward the EARLIEST ref.touched "open"
	// event it has ingested, and ingestion runs for every run as its outcome
	// becomes known (resumeInterruptedRunsWithRunners's own doc comment), so
	// a later-starting run can only ever find a LATER open event, never an
	// earlier one — the milestone this loop looks for is monotonic once set.
	if !firstPROpenAt.IsZero() {
		return telemetry.NewTimeToFirstPRMetric(initCompletedAt, firstPROpenAt), nil
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
	projected, err := s.instanceLog.snapshot(ctx, s.sources.Layout.SchedulerDir())
	if err != nil {
		return SchedulerStatus{}, err
	}
	resetAt := projected.providerQuotaResumeAt
	restart := projected.restart
	refillBlocked := projected.refillBlocked
	if restart != nil {
		restart.Replacements, err = s.restartReplacements(ctx, restart.At)
		if err != nil {
			return SchedulerStatus{}, err
		}
	}
	status := SchedulerStatus{ProviderQuotaResumeAt: resetAt, DaemonRestart: restart}
	for _, key := range projected.refusalOrder {
		status.RefusedWorkflows = append(status.RefusedWorkflows, projected.refusals[key])
	}
	activeCounts, err := s.activeRunCounts()
	if err != nil {
		return SchedulerStatus{}, err
	}
	refill := make([]RefillOccupancyStatus, 0)
	definitions := s.definitions.Load().inventory.definitions
	for _, def := range definitions.Workflows {
		desired := def.Spec.Readiness.DesiredConcurrentRuns
		if desired <= 0 {
			continue
		}
		identity := localscheduler.WorkflowIdentity{Gaggle: def.Spec.Gaggle, Workflow: def.Name}
		occupancy := RefillOccupancyStatus{
			Gaggle:      def.Spec.Gaggle,
			Workflow:    def.Name,
			DesiredRuns: desired,
			ActiveRuns:  int32(activeCounts[identity]),
		}
		if occupancy.ActiveRuns < occupancy.DesiredRuns {
			if blocking, ok := refillBlocked[identity]; ok {
				occupancy.AdmissionBlocked = true
				occupancy.BlockingCondition = blocking
			}
		}
		refill = append(refill, occupancy)
	}
	sort.Slice(refill, func(i, j int) bool {
		if refill[i].Gaggle == refill[j].Gaggle {
			return refill[i].Workflow < refill[j].Workflow
		}
		return refill[i].Gaggle < refill[j].Gaggle
	})
	status.RefillOccupancy = refill

	// Retention diagnostics: expose the effective policy and live loop counters.
	if s.sources.Config != nil {
		retention := RetentionStatus{
			Window: s.sources.Config.ProjectionFullFidelityRetentionDays(),
		}
		if s.sources.RetentionStats != nil {
			stats := s.sources.RetentionStats()
			retention.AgedOut = stats.AgedOut
			retention.Passes = stats.Passes
			if !stats.LastPassAt.IsZero() {
				lastPassAt := stats.LastPassAt
				retention.LastPassAt = &lastPassAt
			}
		}
		status.Retention = &retention
	}

	return status, nil
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
