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

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/signals"
)

const (
	defaultStatusWatchInterval = 2 * time.Second
	statusClearScreen          = "\x1b[H\x1b[2J"
	statusHighlight            = "\x1b[1m"
	statusReset                = "\x1b[0m"
	statusWatchRowFormat       = "%-14.14s  %-18.18s  %-8.8s  %-9.9s  %-20.20s"
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

type statusJSONSummary struct {
	RunID     string    `json:"runId"`
	Workflow  string    `json:"workflow"`
	Gaggle    string    `json:"gaggle"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"startedAt"`
}

type statusJSONOutput struct {
	Warnings []validate.CodedWarning `json:"warnings"`
	Runs     []statusJSONSummary     `json:"runs"`
}

func statusJSONSummaries(runs []runSummary) []statusJSONSummary {
	summaries := make([]statusJSONSummary, len(runs))
	for i, r := range runs {
		summaries[i] = statusJSONSummary{
			RunID:     r.RunID,
			Workflow:  r.Workflow,
			Gaggle:    r.Gaggle,
			Phase:     string(r.Phase),
			StartedAt: r.StartedAt,
		}
	}
	return summaries
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
			RunID:     run.ID,
			Workflow:  run.Workflow,
			Gaggle:    run.Gaggle,
			Phase:     run.Phase,
			StartedAt: run.StartedAt,
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
	jsonOutput := fs.Bool("json", false, "emit config warnings and run summaries as JSON")
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
			pln(stderr, "Status also reports issues parked on learned dependencies and their recorded blockers.")
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

	options := statusOptions{
		phases:   phases,
		workflow: *workflowFilter,
		limit:    *limit,
	}

	loadRuns := func() ([]runSummary, error) {
		return listStatusRuns(context.Background(), reads)
	}
	// Scheduler state is loaded per redraw so watch reflects both quota
	// transitions and backlog-query's learned-dependency refresh.
	loadStatusText := func() (string, error) {
		if !supportsWatch {
			return "", nil
		}
		var text strings.Builder
		status, err := reads.SchedulerStatus(context.Background())
		if err == nil {
			text.WriteString(providerQuotaStatusLine(status, time.Now()))
		}
		parked, err := listParkedDependencies(l)
		if err != nil {
			return "", err
		}
		text.WriteString(parkedDependencyStatusText(parked))
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
	runs = selectStatusRuns(runs, options)
	if *jsonOutput {
		output := statusJSONOutput{
			Warnings: warnings,
			Runs:     statusJSONSummaries(runs),
		}
		if err := json.NewEncoder(stdout).Encode(output); err != nil {
			pf(stderr, "error: encode status: %v\n", err)
			return 2
		}
		return 0
	}

	// Skipped in --json mode (handled above, this point is unreached for
	// it) since the JSON output's shape is a warnings+runs object, not an
	// array with room for a plain-text side channel.
	printValidationWarnings(stdout, warnings)
	statusText, err := loadStatusText()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	pf(stdout, "%s", statusText)
	renderStatus(stdout, runs)
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

func renderStatus(stdout io.Writer, runs []runSummary) {
	if len(runs) == 0 {
		pln(stdout, "no runs found — trigger one with 'goobers run <workflow>'")
		return
	}

	pf(stdout, "%-34s  %-24s  %-10s  %-10s  %s\n", "RUN ID", "WORKFLOW", "GAGGLE", "PHASE", "STARTED")
	for _, r := range runs {
		pf(stdout, "%-34s  %-24s  %-10s  %-10s  %s\n",
			r.RunID, r.Workflow, r.Gaggle, r.Phase, r.StartedAt.Format(time.RFC3339))
	}
}

func watchStatus(
	ctx context.Context,
	interval time.Duration,
	options statusOptions,
	stdout io.Writer,
	loadRuns func() ([]runSummary, error),
	loadStatusText func() (string, error),
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
		statusText, err := loadStatusText()
		if err != nil {
			return err
		}
		renderStatusWatchFrame(stdout, statusText, selectStatusRuns(allRuns, options), changedStatusRuns(previous, current))
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

func renderStatusWatchFrame(stdout io.Writer, statusText string, runs []runSummary, changed map[string]struct{}) {
	pf(stdout, statusClearScreen)
	if statusText != "" {
		pf(stdout, "%s", statusText)
	}
	if len(runs) == 0 {
		pln(stdout, "no runs found — trigger one with 'goobers run <workflow>'")
		return
	}

	pf(stdout, statusWatchRowFormat+"\n", "RUN ID", "WORKFLOW", "GAGGLE", "PHASE", "STARTED")
	for _, run := range runs {
		row := fmt.Sprintf(
			statusWatchRowFormat,
			run.RunID,
			run.Workflow,
			run.Gaggle,
			run.Phase,
			run.StartedAt.Format(time.RFC3339),
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
	activeRuns, err := localscheduler.ActiveRunCounts(l.RunsDir())
	if err != nil {
		pf(stderr, "error: count live runs: %v\n", err)
		return 2
	}
	liveRuns := 0
	for _, count := range activeRuns {
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
