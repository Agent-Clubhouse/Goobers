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
	"strings"
	"time"

	"golang.org/x/term"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/providers"
)

const (
	defaultStatusWatchInterval = 2 * time.Second
	statusProviderQueryTimeout = 30 * time.Second
	statusClearScreen          = "\x1b[H\x1b[2J"
	statusHighlight            = "\x1b[1m"
	statusReset                = "\x1b[0m"
	statusWatchRowFormat       = "%-14.14s  %-18.18s  %-8.8s  %-9.9s  %-20.20s"
	statusFleetRowFormat       = "%-19.19s %-6.6s %-15.15s %-10.10s %s"
	statusSuccessRateWindow    = 10
	statusNextFireScheduled    = "scheduled"
	statusNextFireManual       = "manual"
	statusNextFireEvent        = "event"
)

func providerQuotaStatusLine(status readservice.SchedulerStatus, now time.Time) string {
	if status.ProviderQuotaResumeAt == nil || !now.Before(*status.ProviderQuotaResumeAt) {
		return ""
	}
	return "GitHub quota exhausted — resuming dispatch at " +
		status.ProviderQuotaResumeAt.UTC().Format(time.RFC3339) + "\n"
}

func parkedDependencyStatusText(parked []parkedDependency) string {
	var text strings.Builder
	pf(&text, "Issues parked on learned dependencies: %d\n", len(parked))
	for _, dependency := range parked {
		blockers := make([]string, len(dependency.Blockers))
		for i, blocker := range dependency.Blockers {
			blockers[i] = "#" + blocker
		}
		pf(&text, "  #%s blocked by %s\n", dependency.ItemID, strings.Join(blockers, ", "))
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

var (
	loadStatusPRLabelCounts = queryStatusPRLabelCounts
	newStatusGitHubProvider = providers.NewGitHubProvider
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
	resolver, _, err := buildCredentials(cfg)
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
	prs, err := newStatusGitHubProvider(token).ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: providers.RepositoryRef{
			Provider: providers.ProviderGitHub,
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
	RunID          string    `json:"runId"`
	Workflow       string    `json:"workflow"`
	Gaggle         string    `json:"gaggle"`
	Phase          string    `json:"phase"`
	StartedAt      time.Time `json:"startedAt"`
	LastActivityAt time.Time `json:"lastActivityAt"`
}

type statusJSONOutput struct {
	Warnings []validate.CodedWarning `json:"warnings"`
	Summary  *statusFleetSummary     `json:"summary,omitempty"`
	Runs     []statusJSONSummary     `json:"runs"`
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
	LastOutcome       journal.RunPhase `json:"lastOutcome,omitempty"`
	LastOutcomeAt     *time.Time       `json:"lastOutcomeAt,omitempty"`
	TerminalRuns      int              `json:"terminalRuns"`
	SuccessfulRuns    int              `json:"successfulRuns"`
	SuccessRate       *float64         `json:"successRate"`
	NextFire          statusNextFire   `json:"nextFire"`
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
	pf(stdout, statusFleetRowFormat+"\n", "WORKFLOW", "ACTIVE", "LAST (AGO)", "SUCCESS", "NEXT")
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
			fmt.Sprintf("%d/%d", workflow.InFlight, workflow.MaxConcurrentRuns),
			last,
			success,
			next,
		)
	}
	pf(stdout, "\n")
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
		}
	}
	return runs, nil
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	return runRunTable(args, stdout, stderr, "status")
}

func runRunTable(args []string, stdout, stderr io.Writer, command string) int {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit config warnings, workflow summary, and runs as JSON")
	phaseFilter := fs.String("phase", "", "filter by comma-separated run phases")
	workflowFilter := fs.String("workflow", "", "filter by workflow name")
	limit := fs.Int("limit", 0, "maximum number of runs to show (default: all)")
	// Only `status` supports --daemon, --watch/--interval, and the #712 pause
	// line — all daemon/process runtime state, not part of `runs list`'s
	// plain, scriptable run table.
	supportsWatch := command == "status"
	var watch *bool
	var interval *time.Duration
	var daemon *bool
	if supportsWatch {
		watch = fs.Bool("watch", false, "refresh the status board until interrupted")
		interval = fs.Duration("interval", defaultStatusWatchInterval, "watch refresh interval")
		daemon = fs.Bool("daemon", false, "report daemon health and identity")
	}
	fs.Usage = func() {
		if supportsWatch {
			pf(stderr, "Usage: goobers %s [--daemon | --json] [--phase=<phase>[,<phase>...]] [--workflow=<name>] [--limit=N] [--watch [--interval=2s]] [path]\n\n",
				command)
		} else {
			pf(stderr, "Usage: goobers %s [--json] [--phase=<phase>[,<phase>...]] [--workflow=<name>] [--limit=N] [path]\n\n",
				command)
			pln(stderr, "Alias for the goobers status run table, with the same flags (minus --daemon/--watch).")
		}
		pf(stderr, "Validate active config, show warnings, and list runs under an instance's\n"+
			"runs/ directory with their current phase, newest first (default path \".\").\n")
		if supportsWatch {
			pln(stderr, "Status also reports parked issue dependencies and separate blocked-on-sibling/merge-escalated PR counts.")
			pln(stderr, "With --daemon, report daemon health instead.")
		}
		pf(stderr, "Exit codes: 0 = OK, 1 = validation errors, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
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
	if supportsWatch && *daemon && (*jsonOutput || *phaseFilter != "" || *workflowFilter != "" || *limit != 0 || *watch) {
		pf(stderr, "error: --daemon cannot be combined with run-listing flags\n")
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
	warnings := report.CLIWarnings()
	if _, err := compiledMachines(set, goobersByName(set)); err != nil {
		printValidationWarnings(stderr, warnings)
		pf(stderr, "error: invalid workflow: %v\n", err)
		return 1
	}
	reads, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      l,
		Config:      cfg,
		Definitions: set,
		Validation:  report,
	}, func() bool { return true })
	if err != nil {
		pf(stderr, "error: initialize read service: %v\n", err)
		return 2
	}
	var statusLocation *time.Location
	if supportsWatch {
		statusLocation, err = cfg.Location()
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	}

	options := statusOptions{
		phases:   phases,
		workflow: *workflowFilter,
		limit:    *limit,
	}

	loadRuns := func() ([]runSummary, error) {
		return listStatusRuns(context.Background(), reads)
	}
	loadFleetSummary := func(runs []runSummary, now time.Time) (statusFleetSummary, error) {
		lastEvals, err := statusWorkflowLastEvals(l)
		if err != nil {
			return statusFleetSummary{}, err
		}
		return buildStatusFleetSummary(set.Workflows, runs, lastEvals, now, statusLocation)
	}
	prLabelCounts := newStatusPRLabelCountCache()
	// Scheduler state is loaded per redraw so watch reflects both quota
	// transitions and backlog-query's learned-dependency refresh. Provider PR
	// counts use the scheduler's coarser PR refresh cadence to keep watch API
	// traffic bounded.
	loadStatusText := func(ctx context.Context, runs []runSummary, now time.Time) (string, error) {
		if !supportsWatch {
			return "", nil
		}
		var text strings.Builder
		summary, err := loadFleetSummary(runs, now)
		if err != nil {
			return "", err
		}
		renderStatusFleetSummary(&text, summary, now)
		status, err := reads.SchedulerStatus(context.Background())
		if err == nil {
			text.WriteString(providerQuotaStatusLine(status, now))
		}
		parked, err := listParkedDependencies(l)
		if err != nil {
			return "", err
		}
		text.WriteString(parkedDependencyStatusText(parked))
		counts, err := prLabelCounts.Load(ctx, cfg)
		if err != nil {
			text.WriteString(prLabelStatusUnavailableText(err))
			return text.String(), nil
		}
		text.WriteString(prLabelStatusText(counts))
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
	var fleetSummary *statusFleetSummary
	if supportsWatch {
		summary, err := loadFleetSummary(allRuns, now)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		fleetSummary = &summary
	}
	runs = selectStatusRuns(allRuns, options)
	if *jsonOutput {
		output := statusJSONOutput{
			Warnings: warnings,
			Summary:  fleetSummary,
			Runs:     statusJSONSummaries(runs),
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
	return 0
}

func selectStatusRuns(runs []runSummary, options statusOptions) []runSummary {
	filtered := make([]runSummary, 0, len(runs))
	for _, run := range runs {
		if options.workflow != "" && run.Workflow != options.workflow {
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
		filtered = filtered[:options.limit]
	}
	return filtered
}

func renderStatus(stdout io.Writer, runs []runSummary, now time.Time) {
	if len(runs) == 0 {
		pln(stdout, "no runs found — trigger one with 'goobers run <workflow>'")
		return
	}

	pf(stdout, "%-34s  %-24s  %-10s  %-10s  %-20s  %s\n", "RUN ID", "WORKFLOW", "GAGGLE", "PHASE", "STARTED", "LAST ACTIVITY")
	for _, r := range runs {
		pf(stdout, "%-34s  %-24s  %-10s  %-10s  %-20s  %s\n",
			r.RunID, r.Workflow, r.Gaggle, r.Phase, r.StartedAt.Format(time.RFC3339),
			formatLastActivity(now, r.LastActivityAt))
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
		renderStatusWatchFrame(stdout, statusText, selectStatusRuns(allRuns, options), changedStatusRuns(previous, current), now)
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
	running, identity, err := inspectDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"))
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
			pf(stdout, "daemon running: identity unavailable, live runs %d\n", liveRuns)
			return 0
		}
		uptime := now.Sub(identity.StartedAt)
		if uptime < 0 {
			uptime = 0
		}
		pf(stdout, "daemon running: pid %d, uptime %s, version %s, live runs %d\n",
			identity.PID, uptime.Truncate(time.Second), identity.Version, liveRuns)
		return 0
	}
	if identity != nil {
		pf(stdout, "daemon not running (last daemon: pid %d, started %s); version %s, live runs %d\n",
			identity.PID, identity.StartedAt.Format(time.RFC3339), identity.Version, liveRuns)
		return 1
	}
	pf(stdout, "daemon not running; live runs %d\n", liveRuns)
	return 1
}
