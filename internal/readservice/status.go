package readservice

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

const providerQuotaResumePrefix = localscheduler.ReasonProviderQuota + ": resumes at "

// StatusReader is the shared read boundary used by local status adapters.
type StatusReader interface {
	ListStatusRuns(context.Context, StatusRunOptions) ([]RunSummary, error)
	TimeToFirstPR(context.Context) (telemetry.TimeToFirstPRMetric, error)
	SchedulerStatus(context.Context) (SchedulerStatus, error)
}

// StatusRunOptions bounds and scopes the run population needed by a status
// adapter. A zero value preserves the historical exhaustive read.
type StatusRunOptions struct {
	Gaggle   string
	Workflow string
	Phases   []journal.RunPhase
	Limit    int
}

// StatusFleetFact is the bounded run population needed to compute one
// workflow's status summary without reusing the display page.
type StatusFleetFact struct {
	Gaggle       string
	Workflow     string
	ActiveRuns   int
	TerminalRuns []RunSummary
}

// SchedulerStatus is scheduler state projected from the instance journal for
// local status adapters.
type SchedulerStatus struct {
	// IsolationMandates is the effective, operator-owned class floor loaded
	// by this daemon. Nil means no instance mandate is configured.
	IsolationMandates     map[string][]string
	EngineFallbacks       []readmodel.EngineFallback
	ProviderQuotaResumeAt *time.Time
	DaemonRestart         *DaemonRestartStatus
	// RefusedWorkflows are the workflows the startup constraint solve marked
	// unplaceable on the declared runners: inventory (workflow.refused,
	// #2860/dsl-3.0.md §5 checkpoint 3) for the configuration currently in
	// force: the set resets at each daemon start and each accepted config
	// reload, because the scheduler re-journals current refusals at both
	// boundaries. Empty on zero-declaration instances.
	RefusedWorkflows       []WorkflowRefusalStatus
	RefillOccupancy        []RefillOccupancyStatus
	Retention              *RetentionStatus
	Maintenance            *MaintenanceStatus
	WorkerConfigDivergence []WorkerConfigDivergenceStatus
	TelemetryRetention     *TelemetryRetentionStatus
	JournalHealth          *JournalHealthStatus
}

// JournalHealthStatus exposes process-lifetime instance-journal write health.
// It is absent from offline readers, which cannot observe another process's
// volatile counter without pretending the failed journal persisted it.
type JournalHealthStatus struct {
	AppendsDropped uint64 `json:"appendsDropped"`
}

// WorkerConfigDivergenceStatus is the last config-tree comparison reported by
// one worker. Older journals simply project an empty slice.
type WorkerConfigDivergenceStatus struct {
	Worker       string                              `json:"worker"`
	State        journal.WorkerConfigDivergenceState `json:"state"`
	WorkerDigest string                              `json:"workerDigest,omitempty"`
	DaemonDigest string                              `json:"daemonDigest,omitempty"`
	Reason       string                              `json:"reason,omitempty"`
	Message      string                              `json:"message"`
	At           time.Time                           `json:"at"`
}

// TelemetryRetentionStatus is the effective automatic telemetry-retention
// policy plus the latest successfully journaled evaluation of that policy.
type TelemetryRetentionStatus struct {
	Enabled        bool       `json:"enabled"`
	Window         string     `json:"window"`
	MaxRuns        int        `json:"maxRuns"`
	FirstEnable    string     `json:"firstEnable"`
	EnforceAt      *time.Time `json:"enforceAt,omitempty"`
	LastPassAt     *time.Time `json:"lastPassAt,omitempty"`
	LastPassMode   string     `json:"lastPassMode,omitempty"`
	CandidateCount int        `json:"candidateCount"`
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

// MaintenanceStatus is the bounded, canonical read model for a retention
// sweep. It contains counters, not per-candidate history or filesystem paths.
type MaintenanceStatus struct {
	Kind            string     `json:"kind"`
	State           string     `json:"state"`
	Trigger         string     `json:"trigger"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	LastProgressAt  *time.Time `json:"lastProgressAt,omitempty"`
	CurrentPhase    string     `json:"currentPhase,omitempty"`
	Candidates      int        `json:"candidates"`
	Removed         int        `json:"removed"`
	Failures        int        `json:"failures"`
	LastCompletedAt *time.Time `json:"lastCompletedAt,omitempty"`
	LastResult      string     `json:"lastResult,omitempty"`
	ErrorSummary    string     `json:"errorSummary,omitempty"`
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

// ListStatusRuns returns readable runs in display order. Individual malformed
// historical journals are omitted so status remains best-effort. When a read
// model is attached, scope and limit are pushed into its indexed query instead
// of being applied after an exhaustive projection read (#4863).
func (s *Local) ListStatusRuns(ctx context.Context, options StatusRunOptions) ([]RunSummary, error) {
	if s.readModelReads && s.sources.ReadModel != nil {
		return s.listStatusRunsFromReadModel(ctx, options)
	}
	return s.runSummaries(ctx, true)
}

type statusRunQuery struct {
	gaggle           string
	workflow         string
	phases           []journal.RunPhase
	residualWorkflow string
	residualPhases   map[journal.RunPhase]struct{}
}

func normalizeStatusRunQuery(options StatusRunOptions) statusRunQuery {
	query := statusRunQuery{gaggle: options.Gaggle, workflow: options.Workflow, phases: options.Phases}
	// The read model's closed indexed set does not contain workflow by itself
	// or gaggle+workflow+phase. Keep those predicates correct by paging an
	// indexed supported subset until the requested number of matches is found.
	switch {
	case query.workflow != "" && query.gaggle == "":
		query.residualWorkflow = query.workflow
		query.workflow = ""
	case query.workflow != "" && len(query.phases) > 0:
		query.residualPhases = make(map[journal.RunPhase]struct{}, len(query.phases))
		for _, phase := range query.phases {
			query.residualPhases[phase] = struct{}{}
		}
		query.phases = nil
	}
	if len(query.phases) == 0 {
		query.phases = []journal.RunPhase{""}
	}
	return query
}

func (s *Local) listStatusRunsFromReadModel(ctx context.Context, options StatusRunOptions) ([]RunSummary, error) {
	observedAt := s.now()
	query := normalizeStatusRunQuery(options)
	out := make([]RunSummary, 0, options.Limit)
	for _, phase := range query.phases {
		var (
			cursor     readmodel.ListCursor
			phaseCount int
		)
		for {
			pageLimit := options.Limit
			if pageLimit > 0 {
				pageLimit -= phaseCount
			}
			if pageLimit > 200 || pageLimit == 0 && options.Limit > 0 {
				pageLimit = 200
			}
			page, err := s.sources.ReadModel.ListRuns(ctx, readmodel.ListOptions{
				Gaggle: query.gaggle, Workflow: query.workflow, Phase: phase,
				Limit: pageLimit, Cursor: cursor, IncludeNoWork: true,
			})
			if err != nil {
				return nil, err
			}
			for _, row := range page.Runs {
				if query.residualWorkflow != "" && row.Workflow != query.residualWorkflow {
					continue
				}
				if len(query.residualPhases) > 0 {
					if _, ok := query.residualPhases[row.Phase]; !ok {
						continue
					}
				}
				out = append(out, summaryFromReadModel(row, observedAt))
				phaseCount++
			}
			if !page.HasMore || options.Limit > 0 && phaseCount >= options.Limit {
				break
			}
			cursor = page.Next
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if options.Limit > 0 && len(out) > options.Limit {
		out = out[:options.Limit]
	}
	if err := s.decorateOperatorClaims(ctx, out, observedAt); err != nil {
		return nil, err
	}
	return out, nil
}

// StatusFleetFacts returns exact active counts, the latest ten terminal
// outcomes, and the complete leading infra-failure streak for each configured
// workflow. Work is independent of unrelated run history; an unusually long
// current failure streak is necessarily read in full so its reported length
// remains exact.
func (s *Local) StatusFleetFacts(ctx context.Context) ([]StatusFleetFact, error) {
	if !s.readModelReads || s.sources.ReadModel == nil {
		return nil, ErrReadModelUnavailable
	}
	activeRows, err := s.sources.ReadModel.ActiveRunCounts(ctx)
	if err != nil {
		return nil, err
	}
	active := make(map[string]int, len(activeRows))
	for _, row := range activeRows {
		active[row.Gaggle+"\x00"+row.Workflow] = row.Count
	}
	definitions := s.definitions.Load().set.Workflows
	seen := make(map[string]bool)
	facts := make([]StatusFleetFact, 0, len(definitions))
	for _, definition := range definitions {
		key := definition.Spec.Gaggle + "\x00" + definition.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		fact := StatusFleetFact{Gaggle: definition.Spec.Gaggle, Workflow: definition.Name, ActiveRuns: active[key]}
		var cursor readmodel.ListCursor
		breakerSeen := false
		for {
			page, err := s.sources.ReadModel.ListRuns(ctx, readmodel.ListOptions{
				Gaggle: definition.Spec.Gaggle, Workflow: definition.Name,
				Limit: 200, Cursor: cursor, IncludeNoWork: true, OrderBy: readmodel.OrderLastActivity,
			})
			if err != nil {
				return nil, err
			}
			for _, row := range page.Runs {
				if !row.Terminal {
					continue
				}
				summary := summaryFromReadModel(row, s.now())
				fact.TerminalRuns = append(fact.TerminalRuns, summary)
				if !statusInfraFailure(summary) {
					breakerSeen = true
				}
				if len(fact.TerminalRuns) >= 10 && breakerSeen {
					break
				}
			}
			if len(fact.TerminalRuns) >= 10 && breakerSeen || !page.HasMore {
				break
			}
			cursor = page.Next
		}
		facts = append(facts, fact)
	}
	return facts, nil
}

func statusInfraFailure(run RunSummary) bool {
	return run.Phase == journal.PhaseFailed && run.Operator.LatestError != nil &&
		telemetry.ClassifyError(run.Operator.LatestError.Code).InfraFault()
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
	if s.sources.InstanceLogStats != nil {
		stats := s.sources.InstanceLogStats()
		status.JournalHealth = &JournalHealthStatus{AppendsDropped: stats.AppendsDropped}
	}
	for _, worker := range projected.workerDivergenceOrder {
		status.WorkerConfigDivergence = append(status.WorkerConfigDivergence, projected.workerDivergence[worker])
	}
	if s.sources.Config != nil {
		status.IsolationMandates = s.sources.Config.PlacementInventory("").ClassMandates
		status.TelemetryRetention = telemetryRetentionStatus(s.sources.Config, projected.telemetryRetention)
	}
	for _, key := range projected.engineFallbacks.order {
		status.EngineFallbacks = append(status.EngineFallbacks, projected.engineFallbacks.items[key])
	}
	for _, key := range projected.refusalOrder {
		status.RefusedWorkflows = append(status.RefusedWorkflows, projected.refusals[key])
	}
	activeCounts, err := s.activeRunCounts(ctx)
	if err != nil {
		return SchedulerStatus{}, err
	}
	definitions := s.definitions.Load().inventory.definitions
	status.RefillOccupancy = workflowRefillOccupancy(
		definitions.Workflows,
		activeCounts,
		refillBlocked,
	)

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
				at := stats.LastPassAt
				retention.LastPassAt = &at
			}
		}
		status.Retention = &retention
	}
	if s.sources.RetentionStats != nil {
		status.Maintenance = maintenanceStatus(s.sources.RetentionStats())
	}
	return status, nil
}

func telemetryRetentionStatus(config *instance.Config, latest *TelemetryRetentionStatus) *TelemetryRetentionStatus {
	if config == nil {
		return nil
	}
	retentionConfig := instance.TelemetryRetentionConfig{}
	if config.Telemetry.Retention != nil {
		retentionConfig = *config.Telemetry.Retention
	}
	window, err := retentionConfig.WindowDuration()
	if err != nil {
		return nil // validated configs cannot reach this path
	}
	status := TelemetryRetentionStatus{
		Enabled:     retentionConfig.EnabledEffective(),
		Window:      formatTelemetryRetentionWindow(window),
		MaxRuns:     retentionConfig.MaxRunLimit(),
		FirstEnable: retentionConfig.FirstEnable,
	}
	if status.FirstEnable == "" {
		status.FirstEnable = "gracePeriod"
	}
	if latest != nil {
		if status.Enabled {
			status.EnforceAt = latest.EnforceAt
		}
		status.LastPassAt = latest.LastPassAt
		status.LastPassMode = latest.LastPassMode
		status.CandidateCount = latest.CandidateCount
	}
	return &status
}

func formatTelemetryRetentionWindow(window time.Duration) string {
	if window%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", window/(24*time.Hour))
	}
	return window.String()
}

func workflowRefillOccupancy(
	definitions []apiv1.Workflow,
	activeCounts map[localscheduler.WorkflowIdentity]int,
	refillBlocked map[localscheduler.WorkflowIdentity]string,
) []RefillOccupancyStatus {
	refill := make([]RefillOccupancyStatus, 0)
	for _, def := range definitions {
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
	return refill
}

func maintenanceStatus(stats readmodel.RetentionStats) *MaintenanceStatus {
	status := &MaintenanceStatus{
		Kind:         stats.Kind,
		State:        stats.State,
		Trigger:      stats.Trigger,
		CurrentPhase: stats.CurrentPhase,
		Candidates:   stats.Candidates,
		Removed:      stats.Removed,
		Failures:     stats.Failed,
		LastResult:   stats.LastResult,
		ErrorSummary: stats.LastError,
	}
	if !stats.StartedAt.IsZero() {
		value := stats.StartedAt
		status.StartedAt = &value
	}
	if !stats.LastProgressAt.IsZero() {
		value := stats.LastProgressAt
		status.LastProgressAt = &value
	}
	if !stats.LastCompletedAt.IsZero() {
		value := stats.LastCompletedAt
		status.LastCompletedAt = &value
	}
	return status
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
