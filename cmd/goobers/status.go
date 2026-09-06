package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/avexclusion"
	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/fleet"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

const (
	defaultStatusWatchInterval = 2 * time.Second
	statusProviderQueryTimeout = 30 * time.Second
	statusClearScreen          = "\x1b[H\x1b[2J"
	statusHighlight            = "\x1b[1m"
	statusReset                = "\x1b[0m"
	statusWatchRowFormat       = "%-14.14s  %-18.18s  %-8.8s  %-9.9s  %-20.20s"
	statusFleetRowFormat       = "%-19.19s %-7.7s %-15.15s %-10.10s %s"
	statusSuccessRateWindow    = 10
	statusNextFireScheduled    = "scheduled"
	statusNextFireManual       = "manual"
	statusNextFireEvent        = "event"
	statusFirstSuccessRefresh  = 10 * time.Second
	// statusFailureStreakThreshold is the default number of consecutive
	// infra-classified failures a workflow must accumulate before `goobers
	// status` surfaces an alarm line (#4263: 499 consecutive failures over
	// 18h at 26-28/hour went undetected for 14h22m, because the existing
	// success-rate window only ever looks at the last statusSuccessRateWindow
	// terminal runs — it reads identically whether those are old-and-mixed
	// or fresh-and-all-failing). No new configuration surface: tuned against
	// that measured rate so it trips within roughly 45 minutes of onset at
	// the pattern's observed frequency, comfortably above a single ordinary
	// flaky-infra blip's failure count but far short of leaving a gaggle to
	// run dead for most of a day.
	statusFailureStreakThreshold = 20
)

func providerQuotaStatusLine(status readservice.SchedulerStatus, now time.Time) string {
	if status.ProviderQuotaResumeAt == nil || !now.Before(*status.ProviderQuotaResumeAt) {
		return ""
	}
	return "GitHub quota exhausted — resuming dispatch at " +
		status.ProviderQuotaResumeAt.UTC().Format(time.RFC3339) + "\n"
}

// refusedWorkflowStatusLines surfaces the workflows the startup constraint
// solve refused (#2860, dsl-3.0.md §5 checkpoint 3): the daemon is up and
// every other workflow serves, so these lines are the operator's only
// standing signal that a workflow can never run on the declared inventory.
func refusedWorkflowStatusLines(status readservice.SchedulerStatus) string {
	if len(status.RefusedWorkflows) == 0 {
		return ""
	}
	var text strings.Builder
	for _, refusal := range status.RefusedWorkflows {
		scope := refusal.Workflow
		if refusal.Gaggle != "" {
			scope = refusal.Gaggle + "/" + refusal.Workflow
		}
		fmt.Fprintf(&text, "Workflow %s refused (unplaceable on the declared runners: inventory): %s\n", scope, refusal.Reason)
	}
	return text.String()
}

type statusPRLabelCounts struct {
	blockedOnSibling int
	mergeEscalated   int
}

func prLabelStatusText(counts statusPRLabelCounts) string {
	return fmt.Sprintf(
		"Open PRs with %s: %d\nOpen PRs with %s: %d\n",
		blockedOnSiblingLabel,
		counts.blockedOnSibling,
		remediationEscalatedLabel,
		counts.mergeEscalated,
	)
}

func prLabelStatusUnavailableText(err error) string {
	return fmt.Sprintf("Open PR label counts unavailable: %v\n", err)
}

func timeToFirstPRStatusText(metric telemetry.TimeToFirstPRMetric) string {
	switch {
	case metric.InitCompletedAt == nil:
		return "First-run success: waiting for successful init\n"
	case metric.Milliseconds == nil:
		return "First-run success: waiting for first PR\n"
	default:
		elapsed := time.Duration(*metric.Milliseconds) * time.Millisecond
		return fmt.Sprintf("First-run success: first PR in %s\n", elapsed.Truncate(time.Second))
	}
}

func timeToFirstPRStatusUnavailableText(err error) string {
	return fmt.Sprintf("First-run success unavailable: %v\n", err)
}

type statusTimeToFirstPRCache struct {
	load     func(context.Context) (telemetry.TimeToFirstPRMetric, error)
	now      func() time.Time
	loadedAt time.Time
	metric   telemetry.TimeToFirstPRMetric
	err      error
}

func newStatusTimeToFirstPRCache(
	load func(context.Context) (telemetry.TimeToFirstPRMetric, error),
) *statusTimeToFirstPRCache {
	return &statusTimeToFirstPRCache{load: load, now: time.Now}
}

func (c *statusTimeToFirstPRCache) Load(ctx context.Context) (telemetry.TimeToFirstPRMetric, error) {
	if c.metric.Milliseconds != nil {
		return c.metric, c.err
	}
	if c.loadedAt.IsZero() || !c.now().Before(c.loadedAt.Add(statusFirstSuccessRefresh)) {
		c.metric, c.err = c.load(ctx)
		c.loadedAt = c.now()
	}
	return c.metric, c.err
}

var (
	loadStatusPRLabelCounts = queryStatusPRLabelCounts
	newStatusGitHubProvider = providers.NewGitHubProvider
	newStatusGiteaProvider  = providers.NewGiteaProvider
)

type statusPRLabelCountCache struct {
	load     func(context.Context, *instance.Config) (statusPRLabelCounts, error)
	now      func() time.Time
	loadedAt time.Time
	counts   statusPRLabelCounts
	err      error
}

func newStatusPRLabelCountCache() *statusPRLabelCountCache {
	return &statusPRLabelCountCache{
		load: loadStatusPRLabelCounts,
		now:  time.Now,
	}
}

func (c *statusPRLabelCountCache) Load(ctx context.Context, cfg *instance.Config) (statusPRLabelCounts, error) {
	if c.loadedAt.IsZero() || !c.now().Before(c.loadedAt.Add(localscheduler.DefaultOpenPRRefreshInterval)) {
		c.counts, c.err = c.load(ctx, cfg)
		c.loadedAt = c.now()
	}
	return c.counts, c.err
}

func queryStatusPRLabelCounts(ctx context.Context, cfg *instance.Config) (statusPRLabelCounts, error) {
	if len(cfg.Repos) == 0 {
		return statusPRLabelCounts{}, errors.New("no target repository configured")
	}
	// One-shot query scope: its own composition root, so it builds its own
	// store registry (#683); the surrounding label-count cache already bounds
	// how often this path re-resolves. nil registrar: status is a read-only
	// display path that writes no journal — the same preflight posture as
	// validate's reachability check. nil additionalRepos: this instance-level
	// display path resolves only the primary repo's labels.
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return statusPRLabelCounts{}, err
	}
	resolver, _, err := buildCredentials(cfg, stores, "", "", nil, nil)
	if err != nil {
		return statusPRLabelCounts{}, err
	}
	repo := cfg.Repos[0]
	ref := repo.Owner + "/" + repo.Name
	ctx, cancel := context.WithTimeout(ctx, statusProviderQueryTimeout)
	defer cancel()
	token, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return statusPRLabelCounts{}, fmt.Errorf("resolve status token for %s: %w", ref, err)
	}
	providerKind := providers.ProviderKind(repo.Provider)
	if providerKind == "" {
		providerKind = providers.ProviderGitHub
	}
	var provider providers.RepoProvider
	switch providerKind {
	case providers.ProviderGitHub:
		provider = newStatusGitHubProvider(token)
	case providers.ProviderGitea:
		if repo.BaseURL == "" {
			return statusPRLabelCounts{}, fmt.Errorf("gitea repo %s has no baseUrl configured", ref)
		}
		provider = newStatusGiteaProvider(repo.BaseURL, token)
	default:
		return statusPRLabelCounts{}, fmt.Errorf("open PR label counts do not support repository provider %q", providerKind)
	}
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: providers.RepositoryRef{
			Provider: providerKind,
			Owner:    repo.Owner,
			Name:     repo.Name,
		},
		SkipCheckState: true,
	})
	if err != nil {
		return statusPRLabelCounts{}, fmt.Errorf("list open pull requests for %s: %s", ref, scrubRepositoryError(err, token))
	}

	var counts statusPRLabelCounts
	for _, pr := range prs {
		for _, label := range pr.Labels {
			switch label {
			case blockedOnSiblingLabel:
				counts.blockedOnSibling++
			case remediationEscalatedLabel:
				counts.mergeEscalated++
			}
		}
	}
	return counts, nil
}

type statusJSONSummary struct {
	RunID          string                         `json:"runId"`
	Workflow       string                         `json:"workflow"`
	Gaggle         string                         `json:"gaggle"`
	Phase          string                         `json:"phase"`
	StartedAt      time.Time                      `json:"startedAt"`
	LastActivityAt time.Time                      `json:"lastActivityAt"`
	Operator       readservice.OperatorRunSummary `json:"operator"`
}

type statusJSONOutput struct {
	Warnings      []validate.CodedWarning          `json:"warnings"`
	TimeToFirstPR *telemetry.TimeToFirstPRMetric   `json:"timeToFirstPR,omitempty"`
	DaemonRestart *readservice.DaemonRestartStatus `json:"daemonRestart,omitempty"`
	// RefusedWorkflows are the workflows the startup constraint solve marked
	// unplaceable on the declared runners: inventory (#2860, dsl-3.0.md §5
	// checkpoint 3) — the scripting-side counterpart of the text renderer's
	// refusedWorkflowStatusLines; omitted on zero-declaration instances.
	RefusedWorkflows []readservice.WorkflowRefusalStatus `json:"refusedWorkflows,omitempty"`
	Summary          *statusFleetSummary                 `json:"summary,omitempty"`
	// ParkedBacklog reports items that left the ready pool on a park
	// disposition (#3355); omitted when the provider snapshot is unavailable,
	// the same posture as timeToFirstPR.
	ParkedBacklog *statusParkedBacklog `json:"parkedBacklog,omitempty"`
	// BaselineBlockers reports the shared baseline failures runs are parked on
	// (#2971) — which target-branch CI failure is holding which subjects.
	// Omitted when the local baseline store cannot be read.
	BaselineBlockers *statusBaselineBlockers `json:"baselineBlockers,omitempty"`
	Runs             []statusJSONSummary     `json:"runs"`
}

func daemonRestartStatusLine(status readservice.SchedulerStatus, now time.Time) string {
	restart := status.DaemonRestart
	if restart == nil {
		return ""
	}
	runs := "none"
	if len(restart.RunIDs) > 0 {
		runs = strings.Join(restart.RunIDs, ", ")
	}
	var text strings.Builder
	fmt.Fprintf(&text,
		"Daemon restarted %s (%s); runs resumed/reclaimed: %s\n",
		formatLastActivity(now, restart.At), restart.Reason, runs,
	)
	for _, replacement := range restart.Replacements {
		fmt.Fprintf(
			&text,
			"Warning: run %s failed during the daemon restart and was replaced by %s for item %s\n",
			replacement.FailedRunID,
			replacement.ReplacementRunID,
			replacement.ItemID,
		)
	}
	return text.String()
}

type statusFleetSummary struct {
	SuccessRateWindow int                     `json:"successRateWindow"`
	Workflows         []statusWorkflowSummary `json:"workflows"`
}

type statusWorkflowSummary struct {
	Workflow          string           `json:"workflow"`
	Gaggle            string           `json:"gaggle"`
	InFlight          int              `json:"inFlight"`
	MaxConcurrentRuns int              `json:"maxConcurrentRuns"`
	DesiredRuns       int              `json:"desiredRuns,omitempty"`
	AdmissionBlocked  string           `json:"admissionBlocked,omitempty"`
	LastOutcome       journal.RunPhase `json:"lastOutcome,omitempty"`
	LastOutcomeAt     *time.Time       `json:"lastOutcomeAt,omitempty"`
	TerminalRuns      int              `json:"terminalRuns"`
	SuccessfulRuns    int              `json:"successfulRuns"`
	SuccessRate       *float64         `json:"successRate"`
	NextFire          statusNextFire   `json:"nextFire"`
	// FailureStreak is non-nil only once the streak reaches
	// statusFailureStreakThreshold (#4263) — most callers should treat a nil
	// streak as "no alarm", not "no failures".
	FailureStreak *statusFailureStreak `json:"failureStreak,omitempty"`
}

// statusFailureStreak names a run of consecutive infra-classified failures
// for a workflow (#4263), computed over its full terminal-run history rather
// than the fixed statusSuccessRateWindow — a sustained streak must not go
// blind once it ages past whatever window a success-rate ratio happens to
// use. FirstFailedAt/FirstError name the OLDEST run in the streak, since
// that is when and why the sustained failure actually began, not just its
// most recent recurrence.
type statusFailureStreak struct {
	Length        int       `json:"length"`
	FirstFailedAt time.Time `json:"firstFailedAt"`
	FirstError    string    `json:"firstError"`
}

// statusWorkflowFailureStreak scans terminal runs, most-recent-first, and
// counts the unbounded run of consecutive infra-classified failures at the
// head of the list. A single success or non-infra failure ends the streak
// immediately (#4263's "a single interleaved success clears the alarm").
// Returns nil below statusFailureStreakThreshold.
func statusWorkflowFailureStreak(terminal []runSummary) *statusFailureStreak {
	length := 0
	var oldest runSummary
	for _, run := range terminal {
		if run.Phase != journal.PhaseFailed || run.Operator.LatestError == nil ||
			!telemetry.ClassifyError(run.Operator.LatestError.Code).InfraFault() {
			break
		}
		length++
		oldest = run
	}
	if length < statusFailureStreakThreshold {
		return nil
	}
	return &statusFailureStreak{
		Length:        length,
		FirstFailedAt: statusRunOutcomeTime(oldest),
		FirstError:    statusErrorMessage(oldest.Operator.LatestError),
	}
}

// statusErrorMessage prefers the human-readable message the runner attached
// to the error, falling back to the stable machine code when the runner
// didn't supply one — the same fallback readservice.eventErrorReason uses
// for terminal-run reasons, kept local since it's six lines and the two
// packages already avoid coupling on internal helpers.
func statusErrorMessage(detail *journal.ErrorDetail) string {
	if detail == nil {
		return ""
	}
	if detail.Message != "" {
		return detail.Message
	}
	return detail.Code
}

type statusNextFire struct {
	Kind string     `json:"kind"`
	At   *time.Time `json:"at,omitempty"`
}

func statusJSONSummaries(runs []runSummary) []statusJSONSummary {
	summaries := make([]statusJSONSummary, len(runs))
	for i, r := range runs {
		summaries[i] = statusJSONSummary{
			RunID:          r.RunID,
			Workflow:       r.Workflow,
			Gaggle:         r.Gaggle,
			Phase:          string(r.Phase),
			StartedAt:      r.StartedAt,
			LastActivityAt: r.LastActivityAt,
			Operator:       r.Operator,
		}
	}
	return summaries
}

type statusWorkflowKey struct {
	gaggle   string
	workflow string
}

func buildStatusFleetSummary(
	workflows []apiv1.Workflow,
	runs []runSummary,
	lastEvals map[localscheduler.WorkflowIdentity]time.Time,
	refill map[localscheduler.WorkflowIdentity]readservice.RefillOccupancyStatus,
	now time.Time,
	loc *time.Location,
) (statusFleetSummary, error) {
	runsByWorkflow := make(map[statusWorkflowKey][]runSummary)
	for _, run := range runs {
		key := statusWorkflowKey{gaggle: run.Gaggle, workflow: run.Workflow}
		runsByWorkflow[key] = append(runsByWorkflow[key], run)
	}

	sortedWorkflows := append([]apiv1.Workflow(nil), workflows...)
	sort.Slice(sortedWorkflows, func(i, j int) bool {
		if sortedWorkflows[i].Spec.Gaggle == sortedWorkflows[j].Spec.Gaggle {
			return sortedWorkflows[i].Name < sortedWorkflows[j].Name
		}
		return sortedWorkflows[i].Spec.Gaggle < sortedWorkflows[j].Spec.Gaggle
	})

	summary := statusFleetSummary{
		SuccessRateWindow: statusSuccessRateWindow,
		Workflows:         make([]statusWorkflowSummary, 0, len(sortedWorkflows)),
	}
	for i := range sortedWorkflows {
		def := &sortedWorkflows[i]
		identity := localscheduler.WorkflowIdentity{Gaggle: def.Spec.Gaggle, Workflow: def.Name}
		lastEval := lastEvals[identity]
		if lastEval.IsZero() {
			lastEval = now
		}
		nextFire, err := statusWorkflowNextFire(def, lastEval, loc)
		if err != nil {
			return statusFleetSummary{}, fmt.Errorf("workflow %q: %w", def.Name, err)
		}
		maxConcurrent := int(def.Spec.Readiness.MaxConcurrentRuns)
		if maxConcurrent <= 0 {
			maxConcurrent = 1
		}
		workflowSummary := statusWorkflowSummary{
			Workflow:          def.Name,
			Gaggle:            def.Spec.Gaggle,
			MaxConcurrentRuns: maxConcurrent,
			NextFire:          nextFire,
		}
		if occupancy, ok := refill[identity]; ok {
			workflowSummary.DesiredRuns = int(occupancy.DesiredRuns)
			if occupancy.AdmissionBlocked {
				workflowSummary.AdmissionBlocked = occupancy.BlockingCondition
			}
		}

		var terminal []runSummary
		for _, run := range runsByWorkflow[statusWorkflowKey{gaggle: def.Spec.Gaggle, workflow: def.Name}] {
			if run.Phase == journal.PhaseRunning {
				workflowSummary.InFlight++
				continue
			}
			if statusPhaseIsTerminal(run.Phase) {
				terminal = append(terminal, run)
			}
		}
		sort.Slice(terminal, func(i, j int) bool {
			left := statusRunOutcomeTime(terminal[i])
			right := statusRunOutcomeTime(terminal[j])
			if left.Equal(right) {
				if terminal[i].StartedAt.Equal(terminal[j].StartedAt) {
					return terminal[i].RunID < terminal[j].RunID
				}
				return terminal[i].StartedAt.After(terminal[j].StartedAt)
			}
			return left.After(right)
		})
		if len(terminal) > 0 {
			lastOutcomeAt := statusRunOutcomeTime(terminal[0])
			workflowSummary.LastOutcome = terminal[0].Phase
			workflowSummary.LastOutcomeAt = &lastOutcomeAt
		}
		// Computed over the full terminal history, before windowing below —
		// a sustained streak (#4263) must not depend on a window sized for a
		// success-rate ratio.
		workflowSummary.FailureStreak = statusWorkflowFailureStreak(terminal)
		if len(terminal) > statusSuccessRateWindow {
			terminal = terminal[:statusSuccessRateWindow]
		}
		workflowSummary.TerminalRuns = len(terminal)
		for _, run := range terminal {
			if run.Phase == journal.PhaseCompleted {
				workflowSummary.SuccessfulRuns++
			}
		}
		if workflowSummary.TerminalRuns > 0 {
			rate := float64(workflowSummary.SuccessfulRuns) / float64(workflowSummary.TerminalRuns)
			workflowSummary.SuccessRate = &rate
		}
		summary.Workflows = append(summary.Workflows, workflowSummary)
	}
	return summary, nil
}

func statusWorkflowLastEvals(
	layout instance.Layout,
) (map[localscheduler.WorkflowIdentity]time.Time, error) {
	evaluations, err := localscheduler.ReadTriggerEvaluations(layout.SchedulerDir())
	if err != nil {
		return nil, fmt.Errorf("read scheduler trigger state: %w", err)
	}
	return evaluations, nil
}

func statusWorkflowNextFire(workflow *apiv1.Workflow, lastEval time.Time, loc *time.Location) (statusNextFire, error) {
	schedules := make([]localscheduler.Schedule, 0, len(workflow.Spec.Triggers))
	for _, trigger := range workflow.Spec.Triggers {
		if trigger.Type != apiv1.TriggerSchedule || trigger.Schedule == "" {
			continue
		}
		schedule, err := localscheduler.ParseSchedule(trigger.Schedule)
		if err != nil {
			return statusNextFire{}, err
		}
		schedules = append(schedules, localscheduler.InLocation(schedule, loc))
	}
	if next, ok := localscheduler.NextScheduledFire(schedules, lastEval); ok {
		return statusNextFire{Kind: statusNextFireScheduled, At: &next}, nil
	}
	if len(workflow.Spec.Triggers) == 1 && workflow.Spec.Triggers[0].Type == apiv1.TriggerManual {
		return statusNextFire{Kind: statusNextFireManual}, nil
	}
	return statusNextFire{Kind: statusNextFireEvent}, nil
}

func statusPhaseIsTerminal(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	default:
		return false
	}
}

func statusRunOutcomeTime(run runSummary) time.Time {
	if !run.LastActivityAt.IsZero() {
		return run.LastActivityAt
	}
	return run.StartedAt
}

func renderStatusFleetSummary(stdout io.Writer, summary statusFleetSummary, now time.Time) {
	pf(stdout, "Workflow summary (success rate over last %d terminal runs):\n", summary.SuccessRateWindow)
	pf(stdout, statusFleetRowFormat+"\n", "WORKFLOW", "A/D/MAX", "LAST (AGO)", "SUCCESS", "NEXT")
	nameCounts := make(map[string]int, len(summary.Workflows))
	for _, workflow := range summary.Workflows {
		nameCounts[workflow.Workflow]++
	}
	for _, workflow := range summary.Workflows {
		name := workflow.Workflow
		if nameCounts[name] > 1 {
			name = workflow.Gaggle + "/" + name
		}
		last := "-"
		if workflow.LastOutcomeAt != nil {
			last = fmt.Sprintf("%s %s", workflow.LastOutcome, formatSummaryAge(now, *workflow.LastOutcomeAt))
		}
		success := "-"
		if workflow.SuccessRate != nil {
			success = fmt.Sprintf("%d/%d %.0f%%", workflow.SuccessfulRuns, workflow.TerminalRuns, *workflow.SuccessRate*100)
		}
		next := workflow.NextFire.Kind
		if workflow.NextFire.At != nil {
			next = workflow.NextFire.At.Format(time.RFC3339)
		}
		pf(stdout, statusFleetRowFormat+"\n",
			name,
			statusConcurrencyText(workflow),
			last,
			success,
			next,
		)
		if workflow.AdmissionBlocked != "" {
			pf(stdout, "  %-19.19s blocked: %.45s\n", name, workflow.AdmissionBlocked)
		}
		if streak := workflow.FailureStreak; streak != nil {
			pf(stdout, "ALARM: %s has failed %d consecutive times (infra) since %s: %.80s\n",
				name, streak.Length, streak.FirstFailedAt.UTC().Format(time.RFC3339), streak.FirstError)
		}
	}
	pf(stdout, "\n")
}

func statusConcurrencyText(workflow statusWorkflowSummary) string {
	if workflow.DesiredRuns > 0 {
		return fmt.Sprintf("%d/%d/%d", workflow.InFlight, workflow.DesiredRuns, workflow.MaxConcurrentRuns)
	}
	return fmt.Sprintf("%d/%d", workflow.InFlight, workflow.MaxConcurrentRuns)
}

func formatSummaryAge(now, activity time.Time) string {
	age := now.Sub(activity)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds", int(age/time.Second))
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
	}
}

type statusOptions struct {
	phases   map[journal.RunPhase]struct{}
	workflow string
	gaggle   string
	limit    int
}

func listStatusRuns(ctx context.Context, reads readservice.StatusReader) ([]runSummary, error) {
	summaries, err := reads.ListStatusRuns(ctx)
	if err != nil {
		return nil, err
	}
	runs := make([]runSummary, len(summaries))
	for i, run := range summaries {
		runs[i] = runSummary{
			RunID:          run.ID,
			Workflow:       run.Workflow,
			Gaggle:         run.Gaggle,
			Phase:          run.Phase,
			StartedAt:      run.StartedAt,
			LastActivityAt: run.LastActivityAt,
			Operator:       run.Operator,
		}
	}
	return runs, nil
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	return runRunTable(args, stdout, stderr, "status")
}

// statusHelp and runsListHelp are the two rendered variants of the shared
// runRunTable help: `status` supports --daemon/--watch and reports the extra
// workflow/PR lines, while `runs list` is the flag-reduced alias. runRunTable
// selects between them via helpUsage(stderr, command) (#1095).
const statusHelp = "Usage: goobers status [--daemon | --agents | --json] [--phase=<phase>[,<phase>...]] [--workflow=<name>] [--gaggle=<name>] [--limit=N] [--watch [--interval=2s]] [path]\n\n" +
	"Validate active config, show warnings, and list runs under an instance's\n" +
	"runs/ directory with their current phase, newest first (default path \".\").\n" +
	"Each run includes work identity, stage liveness, PR trajectory, claim drift, latest error, and review rationale.\n" +
	"Status also reports workflow health and separate blocked-on-sibling/merge-escalated PR counts.\n" +
	"It lists parked backlog items too — open issues carrying a park disposition without\n" +
	"goobers:ready, which backlog selection can no longer see and no workflow re-readies.\n" +
	"Shared baseline failures are listed with the subjects waiting on them: runs parked\n" +
	"because the target branch itself fails CI, all released by one repair to that branch.\n" +
	"With --daemon, report daemon health, identity, and effective behavior settings instead.\n" +
	"With --agents, list only the agentic stages in flight right now, by role and run id.\n" +
	"The --agents answer comes from the runner's own journals, never from a process table,\n" +
	"so it can never match the process asking (no `ps | grep` self-match), and it drops the\n" +
	"invoking run when it is itself a stage. It needs no credentials and makes no provider\n" +
	"calls, so it is safe to run from inside a container during a deploy window. Combine it\n" +
	"with --json for scripting, or --workflow/--gaggle to scope it; --phase, --limit and\n" +
	"--watch are refused because the probe reports only the live moment.\n" +
	"Exit codes: 0 = OK, 1 = validation errors, 2 = usage/IO error.\n"

const runsListHelp = "Usage: goobers runs list [--json] [--phase=<phase>[,<phase>...]] [--workflow=<name>] [--gaggle=<name>] [--limit=N] [path]\n\n" +
	"Alias for the goobers status run table, with the same flags (minus --daemon/--watch).\n" +
	"Validate active config, show warnings, and list runs under an instance's\n" +
	"runs/ directory with their current phase, newest first (default path \".\").\n" +
	"Exit codes: 0 = OK, 1 = validation errors, 2 = usage/IO error.\n"

// statusCompiledHarnessWarnings compiles the workflow set exactly as
// runRunTable's caller already did, threading in the same instance-configured
// agent:model resolver admission uses elsewhere (#4292) so a
// file/keychain/store-sourced credential is visible to `status`'s own
// admission-time model discovery, not just an ambient env var. Folds its own
// and the compile call's error handling into one non-zero exit code, so
// runRunTable itself gains a single branch rather than two.
func statusCompiledHarnessWarnings(
	cfg *instance.Config,
	configDir string,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
	cliWarnings []validate.CodedWarning,
	stderr io.Writer,
) ([]gooberHarnessWarning, int) {
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		pf(stderr, "error: invalid secretStores: %v\n", err)
		return nil, 1
	}
	modelCredential, err := agentModelCredentialResolver(cfg, stores)
	if err != nil {
		pf(stderr, "error: invalid credentials: %v\n", err)
		return nil, 1
	}
	_, _, _, harnessWarnings, err := compiledMachinesWithGooberDigestsAndWarnings(
		configDir, set, goobers, instructions, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand,
		false, modelCredential,
	)
	if err != nil {
		printValidationWarnings(stderr, cliWarnings)
		pf(stderr, "error: invalid workflow: %v\n", err)
		return nil, 1
	}
	return harnessWarnings, 0
}

func runRunTable(args []string, stdout, stderr io.Writer, command string) int {
	fs := newCLIFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit config warnings, workflow summary, and runs as JSON")
	phaseFilter := fs.String("phase", "", "filter by comma-separated run phases")
	workflowFilter := fs.String("workflow", "", "filter by workflow name")
	gaggleFilter := fs.String("gaggle", "", "filter by gaggle name")
	limit := fs.Int("limit", 50, "maximum number of runs to show (default: 50; 0 for all)")
	// Only `status` supports --daemon, --watch/--interval, and the #712 pause
	// line — all daemon/process runtime state, not part of `runs list`'s
	// plain, scriptable run table.
	supportsWatch := command == "status"
	var watch *bool
	var interval *time.Duration
	var daemon *bool
	var agents *bool
	if supportsWatch {
		watch = fs.Bool("watch", false, "refresh the status board until interrupted")
		interval = fs.Duration("interval", defaultStatusWatchInterval, "watch refresh interval")
		daemon = fs.Bool("daemon", false, "report daemon health and identity")
		agents = fs.Bool("agents", false, "list in-flight agentic stages by role, from the runner's own bookkeeping")
	}
	fs.Usage = helpUsage(stderr, command)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	limitSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "limit" {
			limitSet = true
		}
	})
	if *limit < 0 {
		pf(stderr, "error: --limit must be non-negative\n")
		return 2
	}
	if supportsWatch && *interval <= 0 {
		pf(stderr, "error: --interval must be greater than zero\n")
		return 2
	}
	if supportsWatch && *watch && *jsonOutput {
		pf(stderr, "error: --watch cannot be used with --json\n")
		return 2
	}
	if supportsWatch && *daemon && (*jsonOutput || *phaseFilter != "" || *workflowFilter != "" || *gaggleFilter != "" || limitSet || *watch || *agents) {
		pf(stderr, "error: --daemon cannot be combined with run-listing flags\n")
		return 2
	}
	// --agents answers one question — which agentic stages are in flight right
	// now — so the flags that shape the historical run table (--phase, --limit)
	// and the redraw loop (--watch) are refused rather than silently ignored.
	// --workflow/--gaggle stay available: scoping the probe to one workflow is
	// the same question asked of a smaller fleet.
	agentsMode := supportsWatch && *agents
	if agentsMode && (*phaseFilter != "" || limitSet || *watch) {
		pf(stderr, "error: --agents cannot be combined with --phase, --limit, or --watch\n")
		return 2
	}

	phases := make(map[journal.RunPhase]struct{})
	if *phaseFilter != "" {
		for _, value := range strings.Split(*phaseFilter, ",") {
			phase := journal.RunPhase(strings.TrimSpace(value))
			switch phase {
			case journal.PhaseRunning, journal.PhaseCompleted, journal.PhaseFailed,
				journal.PhaseAborted, journal.PhaseEscalated:
				phases[phase] = struct{}{}
			default:
				pf(stderr, "error: invalid phase %q (want running, completed, failed, aborted, or escalated)\n", value)
				return 2
			}
		}
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if supportsWatch && *watch && !statusOutputIsTerminal(stdout) {
		pf(stderr, "error: --watch requires terminal stdout; omit --watch when piping status output\n")
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}
	if supportsWatch && *daemon {
		return reportDaemonStatus(l, time.Now(), stdout, stderr)
	}

	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		pf(stderr, "error: invalid instance.yaml: %v\n", err)
		return 1
	}
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		printValidationIssues(stderr, report)
		if errors.Is(err, instance.ErrInvalidConfig) {
			pf(stderr, "error: config directory failed validation\n")
			return 1
		}
		pf(stderr, "error: %v\n", err)
		return 2
	}
	goobers := goobersByName(set)
	instructions, err := loadGooberInstructions(l.ConfigDir(), goobers)
	if err != nil {
		printValidationWarnings(stderr, report.CLIWarnings())
		pf(stderr, "error: invalid workflow: %v\n", err)
		return 1
	}
	harnessWarnings, code := statusCompiledHarnessWarnings(cfg, l.ConfigDir(), set, goobers, instructions, report.CLIWarnings(), stderr)
	if code != 0 {
		return code
	}
	if _, err := appendGooberHarnessWarnings(report, harnessWarnings); err != nil {
		pf(stderr, "error: append harness validation warnings: %v\n", err)
		return 2
	}
	warnings := report.CLIWarnings()
	sources := readservice.LocalSources{
		Layout:      l,
		Config:      cfg,
		Definitions: set,
		Validation:  report,
	}
	// The agents probe answers from local bookkeeping only. Leaving the
	// provider work-item lookup unset keeps it credential-free and network-free
	// — the operator running it through `kubectl exec` during a deploy window
	// gets the same answer as the daemon host, and cannot be told that the
	// diagnostic's own missing credential is a run blocker (#3346).
	if !agentsMode {
		sources.WorkItemLookup = statusWorkItemLookup(l.Root, set)
	}
	livenessTimeout, err := cfg.Runner.LivenessTimeoutDuration()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	sources.LivenessTimeout = livenessTimeout
	var timeToFirstPROpenErr error
	if supportsWatch && !agentsMode {
		telemetryDB, err := openRollup(l, false)
		if err != nil {
			timeToFirstPROpenErr = fmt.Errorf("open telemetry rollup: %w", err)
		} else {
			defer func() { _ = telemetryDB.Close() }()
			sources.Telemetry = telemetryDB
		}
	}
	reads, err := readservice.NewLocal(sources, func() bool { return true })
	if err != nil {
		pf(stderr, "error: initialize read service: %v\n", err)
		return 2
	}
	var statusLocation *time.Location
	if supportsWatch && !agentsMode {
		statusLocation, err = cfg.Location()
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	}

	options := statusOptions{
		phases:   phases,
		workflow: *workflowFilter,
		gaggle:   *gaggleFilter,
		limit:    *limit,
	}

	loadRuns := func() ([]runSummary, error) {
		return listStatusRuns(context.Background(), reads)
	}
	loadFleetSummary := func(
		runs []runSummary,
		schedulerStatus readservice.SchedulerStatus,
		now time.Time,
	) (statusFleetSummary, error) {
		lastEvals, err := statusWorkflowLastEvals(l)
		if err != nil {
			return statusFleetSummary{}, err
		}
		refill := make(map[localscheduler.WorkflowIdentity]readservice.RefillOccupancyStatus, len(schedulerStatus.RefillOccupancy))
		for _, occupancy := range schedulerStatus.RefillOccupancy {
			refill[localscheduler.WorkflowIdentity{Gaggle: occupancy.Gaggle, Workflow: occupancy.Workflow}] = occupancy
		}
		return buildStatusFleetSummary(set.Workflows, runs, lastEvals, refill, now, statusLocation)
	}
	prLabelCounts := newStatusPRLabelCountCache()
	parkedBacklog := newStatusParkedBacklogCache()
	loadTimeToFirstPR := reads.TimeToFirstPR
	if timeToFirstPROpenErr != nil {
		loadTimeToFirstPR = func(context.Context) (telemetry.TimeToFirstPRMetric, error) {
			return telemetry.TimeToFirstPRMetric{}, timeToFirstPROpenErr
		}
	}
	timeToFirstPRCache := newStatusTimeToFirstPRCache(loadTimeToFirstPR)
	// Scheduler state is loaded per redraw so watch reflects quota transitions.
	// Provider PR counts use the scheduler's coarser PR refresh cadence to keep
	// watch API traffic bounded.
	loadStatusText := func(ctx context.Context, runs []runSummary, now time.Time) (string, error) {
		if !supportsWatch {
			return "", nil
		}
		var text strings.Builder
		timeToFirstPR, err := timeToFirstPRCache.Load(ctx)
		if err != nil {
			text.WriteString(timeToFirstPRStatusUnavailableText(err))
		} else {
			text.WriteString(timeToFirstPRStatusText(timeToFirstPR))
		}
		status, err := reads.SchedulerStatus(context.Background())
		if err == nil {
			summary, summaryErr := loadFleetSummary(runs, status, now)
			if summaryErr != nil {
				return "", summaryErr
			}
			renderStatusFleetSummary(&text, summary, now)
			text.WriteString(daemonRestartStatusLine(status, now))
			text.WriteString(providerQuotaStatusLine(status, now))
			text.WriteString(refusedWorkflowStatusLines(status))
		} else {
			summary, summaryErr := loadFleetSummary(runs, readservice.SchedulerStatus{}, now)
			if summaryErr != nil {
				return "", summaryErr
			}
			renderStatusFleetSummary(&text, summary, now)
		}
		counts, err := prLabelCounts.Load(ctx, cfg)
		if err != nil {
			text.WriteString(prLabelStatusUnavailableText(err))
		} else {
			text.WriteString(prLabelStatusText(counts))
		}
		// Parked backlog items (#3355): a park disposition strips
		// goobers:ready, so these items are gone from the ready pool with
		// nothing configured to put them back. An unavailable PR count must
		// not suppress them — they are the section an unattended instance
		// needs most.
		parked, err := parkedBacklog.Load(ctx, cfg)
		if err != nil {
			text.WriteString(parkedBacklogStatusUnavailableText(err))
		} else {
			text.WriteString(parkedBacklogStatusText(parked))
		}
		// Shared baseline failures (#2971): runs parked because the target
		// branch itself is red. Local state only, so it is rendered whether or
		// not the provider-backed sections above resolved.
		if blockers, err := loadStatusBaselineBlockers(l); err != nil {
			text.WriteString(baselineBlockerStatusUnavailableText(err))
		} else {
			text.WriteString(baselineBlockerStatusText(blockers, now))
		}
		return text.String(), nil
	}
	if supportsWatch && *watch {
		// Config warnings are a static, one-time-per-invocation check (unlike
		// the provider-quota pause, which is live scheduler state) — printed
		// once before entering the redraw loop, not re-shown every tick.
		printValidationWarnings(stdout, warnings)
		ctx, stop := signals.SetupSignalContext()
		defer stop()
		if err := watchStatus(ctx, *interval, options, stdout, loadRuns, loadStatusText); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}

	runs, err := loadRuns()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	allRuns := runs
	now := time.Now()
	if agentsMode {
		probe := buildAgentProbe(allRuns, set.Workflows, selfProbeRunID(), *workflowFilter, *gaggleFilter)
		if *jsonOutput {
			if err := emitAgentProbeJSON(stdout, probe); err != nil {
				pf(stderr, "error: %v\n", err)
				return 2
			}
			return 0
		}
		// Config warnings first, same as the run table: a workflow definition
		// that failed to load is exactly what turns a known role into
		// "unknown", so the reader must see the warning next to the answer.
		printValidationWarnings(stdout, warnings)
		renderAgentProbe(stdout, probe, now)
		return 0
	}
	var fleetSummary *statusFleetSummary
	if supportsWatch {
		status, statusErr := reads.SchedulerStatus(context.Background())
		if statusErr != nil {
			status = readservice.SchedulerStatus{}
		}
		summary, err := loadFleetSummary(allRuns, status, now)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		fleetSummary = &summary
	}
	runs, olderRuns := selectStatusRuns(allRuns, options)
	if *jsonOutput {
		var timeToFirstPR *telemetry.TimeToFirstPRMetric
		var daemonRestart *readservice.DaemonRestartStatus
		var refusedWorkflows []readservice.WorkflowRefusalStatus
		var parked *statusParkedBacklog
		if supportsWatch {
			metric, err := timeToFirstPRCache.Load(context.Background())
			if err == nil {
				timeToFirstPR = &metric
			}
			if status, err := reads.SchedulerStatus(context.Background()); err == nil {
				daemonRestart = status.DaemonRestart
				refusedWorkflows = status.RefusedWorkflows
			}
			if snapshot, err := parkedBacklog.Load(context.Background(), cfg); err == nil {
				parked = &snapshot
			}
		}
		var baselineBlockers *statusBaselineBlockers
		// Omitted when nothing is parked, like the other optional sections: a
		// healthy instance's JSON keeps exactly the shape it had before.
		if snapshot, err := loadStatusBaselineBlockers(l); err == nil && snapshot.Total > 0 {
			baselineBlockers = &snapshot
		}
		output := statusJSONOutput{
			Warnings:         warnings,
			TimeToFirstPR:    timeToFirstPR,
			DaemonRestart:    daemonRestart,
			RefusedWorkflows: refusedWorkflows,
			Summary:          fleetSummary,
			ParkedBacklog:    parked,
			BaselineBlockers: baselineBlockers,
			Runs:             statusJSONSummaries(runs),
		}
		if err := json.NewEncoder(stdout).Encode(output); err != nil {
			pf(stderr, "error: encode status: %v\n", err)
			return 2
		}
		return 0
	}

	// Skipped in --json mode since the structured summary has no plain-text
	// side channel.
	printValidationWarnings(stdout, warnings)
	statusText, err := loadStatusText(context.Background(), allRuns, now)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	pf(stdout, "%s", statusText)
	renderStatus(stdout, runs, now)
	renderOlderRunsHint(stdout, olderRuns)
	return 0
}

func selectStatusRuns(runs []runSummary, options statusOptions) ([]runSummary, int) {
	filtered := make([]runSummary, 0, len(runs))
	for _, run := range runs {
		if options.workflow != "" && run.Workflow != options.workflow {
			continue
		}
		if options.gaggle != "" && run.Gaggle != options.gaggle {
			continue
		}
		if len(options.phases) > 0 {
			if _, ok := options.phases[run.Phase]; !ok {
				continue
			}
		}
		filtered = append(filtered, run)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].StartedAt.Equal(filtered[j].StartedAt) {
			return filtered[i].RunID < filtered[j].RunID
		}
		return filtered[i].StartedAt.After(filtered[j].StartedAt)
	})
	if options.limit > 0 && len(filtered) > options.limit {
		return filtered[:options.limit], len(filtered) - options.limit
	}
	return filtered, 0
}

func renderStatus(stdout io.Writer, runs []runSummary, now time.Time) {
	if len(runs) == 0 {
		pln(stdout, "no runs found — trigger one with 'goobers run <workflow>'")
		return
	}

	pf(stdout, "%-34s  %-24s  %-10s  %-22s  %-14s  %-12s  %-18s  %s\n",
		"RUN ID", "ISSUE", "PHASE", "STAGE / TRAJECTORY", "LAST ACTIVITY", "PR", "CLAIM / MARKER", "NEXT")
	for _, r := range runs {
		issue := "-"
		if r.Operator.Issue != nil {
			issue = "#" + r.Operator.Issue.Number
			if r.Operator.Issue.Title != "" {
				issue += " " + r.Operator.Issue.Title
			}
		}
		pr := "-"
		if r.Operator.PullRequest != nil {
			pr = "#" + r.Operator.PullRequest.ID
		} else if r.Operator.PROpenerStage != "" {
			pr = "via " + r.Operator.PROpenerStage
		}
		heartbeat := "-"
		if r.Operator.LastHeartbeatAt != nil {
			heartbeat = r.Operator.Liveness + " " + formatLastActivity(now, *r.Operator.LastHeartbeatAt)
		} else if r.Phase == journal.PhaseRunning {
			heartbeat = r.Operator.Liveness
		}
		stage := r.Operator.CurrentStage
		if stage == "" {
			stage = "-"
		}
		stage += " / " + r.Operator.Trajectory
		claim := r.Operator.Claim.LeaseStatus + " / " + r.Operator.Claim.ProviderMarker
		pf(stdout, "%-34s  %-24s  %-10s  %-22s  %-14s  %-12s  %-18s  %s\n",
			r.RunID, truncateStatusCell(issue, 24), r.Phase, truncateStatusCell(stage, 22),
			heartbeat, pr, claim, r.Operator.NextTransition)
		pf(stdout, "  workflow: %s / %s; started %s; last activity %s\n",
			r.Gaggle, r.Workflow, r.StartedAt.Format(time.RFC3339), formatLastActivity(now, r.LastActivityAt))
		if r.Operator.Issue != nil && r.Operator.Issue.Title != "" {
			pf(stdout, "  work: #%s %s\n", r.Operator.Issue.Number, r.Operator.Issue.Title)
		}
		if r.Operator.Review != nil && r.Operator.Review.Rationale != "" {
			pf(stdout, "  review %s: %s\n", r.Operator.Review.Verdict, r.Operator.Review.Rationale)
		}
		if len(r.Operator.PotentialBlockers) > 0 {
			pf(stdout, "  blockers: %s\n", strings.Join(r.Operator.PotentialBlockers, "; "))
		}
		// Explicitly disclaimed and rendered after blockers: this line is about
		// what THIS status invocation could not check, not about the run (#3346).
		if len(r.Operator.DiagnosticsLimitations) > 0 {
			pf(stdout, "  diagnostics limited (not a run blocker): %s\n",
				strings.Join(r.Operator.DiagnosticsLimitations, "; "))
		}
	}
}

func truncateStatusCell(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

func renderOlderRunsHint(stdout io.Writer, olderRuns int) {
	if olderRuns > 0 {
		pf(stdout, "%d older runs; use --limit 0 for all\n", olderRuns)
	}
}

func formatLastActivity(now, activity time.Time) string {
	if activity.IsZero() {
		return "-"
	}
	age := now.Sub(activity)
	if age < 0 {
		age = 0
	}
	return age.Truncate(time.Second).String() + " ago"
}

func watchStatus(
	ctx context.Context,
	interval time.Duration,
	options statusOptions,
	stdout io.Writer,
	loadRuns func() ([]runSummary, error),
	loadStatusText func(context.Context, []runSummary, time.Time) (string, error),
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var previous map[string]journal.RunPhase
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		allRuns, err := loadRuns()
		if err != nil {
			return err
		}
		current := statusRunPhases(allRuns)
		now := time.Now()
		statusText, err := loadStatusText(ctx, allRuns, now)
		if err != nil {
			return err
		}
		runs, olderRuns := selectStatusRuns(allRuns, options)
		renderStatusWatchFrame(stdout, statusText, runs, changedStatusRuns(previous, current), now)
		renderOlderRunsHint(stdout, olderRuns)
		previous = current

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func statusRunPhases(runs []runSummary) map[string]journal.RunPhase {
	phases := make(map[string]journal.RunPhase, len(runs))
	for _, run := range runs {
		phases[run.RunID] = run.Phase
	}
	return phases
}

func changedStatusRuns(previous, current map[string]journal.RunPhase) map[string]struct{} {
	changed := make(map[string]struct{})
	for runID, phase := range current {
		if previousPhase, ok := previous[runID]; ok && previousPhase != phase {
			changed[runID] = struct{}{}
		}
	}
	return changed
}

func renderStatusWatchFrame(stdout io.Writer, statusText string, runs []runSummary, changed map[string]struct{}, now time.Time) {
	pf(stdout, statusClearScreen)
	if statusText != "" {
		pf(stdout, "%s", statusText)
	}
	if len(runs) == 0 {
		pln(stdout, "no runs found — trigger one with 'goobers run <workflow>'")
		return
	}

	pf(stdout, statusWatchRowFormat+"\n", "RUN ID", "WORKFLOW", "GAGGLE", "PHASE", "LAST ACTIVITY")
	for _, run := range runs {
		row := fmt.Sprintf(
			statusWatchRowFormat,
			run.RunID,
			run.Workflow,
			run.Gaggle,
			run.Phase,
			formatLastActivity(now, run.LastActivityAt),
		)
		if _, ok := changed[run.RunID]; ok {
			pf(stdout, "%s%s%s\n", statusHighlight, row, statusReset)
			continue
		}
		pln(stdout, row)
	}
}

func statusOutputIsTerminal(stdout io.Writer) bool {
	file, ok := stdout.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

func reportDaemonStatus(l instance.Layout, now time.Time, stdout, stderr io.Writer) int {
	running, identity, liveness, err := inspectDaemonLiveness(filepath.Join(l.SchedulerDir(), "up.lock"), now)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	runDirs, err := l.RunDirs()
	if err != nil {
		pf(stderr, "error: enumerate run journals: %v\n", err)
		return 2
	}
	scopedCounts, err := localscheduler.ActiveRunCountsByWorkflowDirs(runDirs)
	if err != nil {
		pf(stderr, "error: count live runs: %v\n", err)
		return 2
	}
	liveRuns := 0
	for _, count := range scopedCounts {
		liveRuns += count
	}

	if running {
		if identity == nil {
			pf(stdout, "daemon %s: identity unavailable, last tick %s ago, live runs %d\n",
				daemonLivenessLabel(liveness), liveness.Age.Truncate(time.Second), liveRuns)
			if liveness.Healthy {
				return 0
			}
			return 1
		}
		uptime := now.Sub(identity.StartedAt)
		if uptime < 0 {
			uptime = 0
		}
		if !liveness.Healthy {
			pf(stdout, "daemon unhealthy: pid %d, uptime %s, version %s, last tick %s ago (threshold %s), live runs %d\n",
				identity.PID, uptime.Truncate(time.Second), identity.Version,
				liveness.Age.Truncate(time.Second), liveness.Timeout, liveRuns)
			reportDaemonBehavior(stdout, identity.Behavior)
			reportFleetEnrollment(l.Root, stdout)
			reportPendingTriggerQueue(l.SchedulerDir(), now, stdout)
			reportAVExclusionReadiness(l, stdout, realAVExclusionDeps())
			return 1
		}
		pf(stdout, "daemon running: pid %d, uptime %s, version %s, last tick %s ago, live runs %d\n",
			identity.PID, uptime.Truncate(time.Second), identity.Version,
			liveness.Age.Truncate(time.Second), liveRuns)
		reportDaemonBehavior(stdout, identity.Behavior)
		reportFleetEnrollment(l.Root, stdout)
		reportPendingTriggerQueue(l.SchedulerDir(), now, stdout)
		reportAVExclusionReadiness(l, stdout, realAVExclusionDeps())
		return 0
	}
	if identity != nil {
		pf(stdout, "recorded daemon is not running: pid %d, started %s; version %s, live runs %d\n",
			identity.PID, identity.StartedAt.Format(time.RFC3339), identity.Version, liveRuns)
		return 1
	}

	pf(stdout, "daemon not running; live runs %d\n", liveRuns)
	return 1
}

// reportPendingTriggerQueue surfaces #4323's operator-visibility acceptance
// criterion: a growing pending-trigger backlog (like #4326's incident, which
// accumulated 1,177 duplicates before anyone noticed) becomes visible in
// `goobers status` before it starves anything. Silent when the queue is
// empty — an operator scanning routine status output shouldn't have to parse
// a "depth 0" line to know nothing is wrong.
func reportPendingTriggerQueue(schedulerDir string, now time.Time, stdout io.Writer) {
	depth, oldestAge, err := pendingTriggerQueueStats(schedulerDir, now)
	if err != nil || depth == 0 {
		return
	}
	pf(stdout, "pending triggers: %d outstanding, oldest %s\n", depth, oldestAge.Truncate(time.Second))
}

func reportDaemonBehavior(stdout io.Writer, behavior *daemonBehavior) {
	if behavior == nil {
		pln(stdout, "daemon behavior: unavailable (daemon predates behavior reporting)")
		return
	}
	drainTimeout := "unbounded"
	if behavior.DrainTimeoutNanos > 0 {
		drainTimeout = time.Duration(behavior.DrainTimeoutNanos).String()
	}
	memoryHighWater := "disabled"
	if !behavior.MemoryGateDisabled {
		memoryHighWater = strconv.FormatFloat(behavior.MemoryHighWater, 'g', -1, 64)
	}
	pf(stdout,
		"daemon behavior: watch-config=%t, diagnostics=%t, drain-timeout=%s, skip-preflight=%t, disable-read-model-reads=%t, memory-high-water=%s, fsync-disabled=%t\n",
		behavior.WatchConfig,
		behavior.Diagnostics,
		drainTimeout,
		behavior.SkipPreflight,
		behavior.DisableReadModelReads,
		memoryHighWater,
		behavior.FsyncDisabled,
	)
}

// reportFleetEnrollment prints whether this instance is enrolled with a
// Fleet service (#4218). Enrollment is a filesystem-only check
// (fleet.LoadAssociation), independently readable by this process without
// going through the daemon — previously it was visible only via the
// separate `goobers fleet status` subcommand, so an operator reading
// `goobers status` alone had no indication either way.
func reportFleetEnrollment(instanceRoot string, stdout io.Writer) {
	storage, err := newFleetStorage()
	if err != nil {
		pf(stdout, "fleet: unavailable (%v)\n", err)
		return
	}
	association, err := storage.LoadAssociation(instanceRoot)
	if errors.Is(err, fleet.ErrNotAssociated) {
		pln(stdout, "fleet: not enrolled")
		return
	}
	if err != nil {
		pf(stdout, "fleet: unavailable (%v)\n", err)
		return
	}
	pf(stdout, "fleet: enrolled as %q (%s)\n", association.DisplayName, association.CanonicalURI)
}

// reportAVExclusionReadiness surfaces #4416's daemon-health acceptance
// criterion: the AV-exclusion coverage `goobers doctor --av-exclusions`
// already computes is otherwise invisible between one manual doctor run and
// the next, so a real-time-scan handle race (#3161–#3164) can go unnoticed
// until it surfaces as an unrelated git "Permission denied" hours later.
//
// The directory inventory comes from daemonAVExclusionDirectories — the same
// function doctor calls — so this can never enumerate a different set than
// the operator's own tooling sees. Silent off Windows: a Linux or macOS
// daemon has no AV-scan race to report, matching hostAVExclusionAdvisory's
// existing silence at startup.
//
// avexclusion.Summary itself is what keeps this from ever falsely declaring
// exclusions present: when the exclusion list could not be read (Queried
// false — no Defender, PowerShell unavailable, a transient query failure) it
// reports "could not read Microsoft Defender exclusions" rather than
// collapsing that into either verdict, so an operator on an unverified host
// sees exactly that — not a false all-clear.
func reportAVExclusionReadiness(l instance.Layout, stdout io.Writer, deps avExclusionDeps) {
	if deps.hostOS != "windows" {
		return
	}
	var cfg *instance.Config
	if _, statErr := os.Stat(l.ConfigFile()); statErr == nil {
		if loaded, loadErr := instance.LoadConfig(l.ConfigFile()); loadErr == nil {
			cfg = loaded
		}
	}
	var set *instance.ConfigSet
	if _, statErr := os.Stat(l.ConfigDir()); statErr == nil {
		loaded, configReport, loadErr := instance.LoadConfigDirForComparison(l.ConfigDir())
		switch {
		case loaded != nil:
			set = loaded
			// The gaggles parsed but the directory does not validate — say so
			// rather than silently reporting coverage over a per-gaggle
			// inventory the daemon itself would refuse (mirrors doctor's own
			// note, runDoctorAVExclusions above).
			if summary := validationIssueSummary(configReport); summary != "" {
				pf(stdout, "av-exclusions (advisory, daemon): note: %s does not validate (%s); per-gaggle workcopies roots below are read from it as-is\n", l.ConfigDir(), summary)
			}
		case loadErr != nil:
			pf(stdout, "av-exclusions (advisory, daemon): note: %s could not be loaded (%v); per-gaggle workcopies roots are NOT enumerated below\n", l.ConfigDir(), loadErr)
		}
	}
	dirs := daemonAVExclusionDirectories(l, cfg, set, deps)
	report := avExclusionReport(context.Background(), dirs, deps)
	pln(stdout, avexclusion.Summary("daemon", report))
}

func daemonLivenessLabel(liveness daemonstate.Liveness) string {
	if liveness.Healthy {
		return "running"
	}
	return "unhealthy"
}
