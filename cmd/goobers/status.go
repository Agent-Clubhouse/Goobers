package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/signals"
)

const (
	defaultStatusWatchInterval = 2 * time.Second
	statusClearScreen          = "\x1b[H\x1b[2J"
	statusHighlight            = "\x1b[1m"
	statusReset                = "\x1b[0m"
	statusWatchRowFormat       = "%-14.14s  %-18.18s  %-8.8s  %-9.9s  %-20.20s"
)

// providerQuotaResumePrefix matches the Reason string Conditions.Admit
// writes for a #712 provider-quota skip (internal/localscheduler/
// conditions.go): the stable ReasonProviderQuota prefix, plus the resume
// time this package needs to recover from the journal — `goobers status` is
// a separate process invocation from the live daemon, so it can't read
// ProviderQuotaState's in-memory value directly and must reconstruct it from
// what was durably journaled.
const providerQuotaResumePrefix = localscheduler.ReasonProviderQuota + ": resumes at "

// parseProviderQuotaResumeTime extracts the resume time from a tick.skipped
// event's Reason, when it's a #712 provider-quota skip. ok is false for any
// other reason string, or one that doesn't parse.
func parseProviderQuotaResumeTime(reason string) (t time.Time, ok bool) {
	if !strings.HasPrefix(reason, providerQuotaResumePrefix) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimPrefix(reason, providerQuotaResumePrefix))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// providerQuotaStatusLine scans the instance journal's events (in seq order)
// for the most recent tick.skipped provider-quota skip and, only while its
// resume time is still ahead of now, renders a status line for it — "" when
// the quota was never recorded exhausted, or its resume time already passed
// (dispatch has resumed; no line needed).
func providerQuotaStatusLine(events []journal.Event, now time.Time) string {
	var resetAt time.Time
	var found bool
	for _, ev := range events {
		if ev.Type != journal.EventTickSkipped {
			continue
		}
		if t, ok := parseProviderQuotaResumeTime(ev.Reason); ok {
			resetAt, found = t, true
		}
	}
	if !found || !now.Before(resetAt) {
		return ""
	}
	return "GitHub quota exhausted — resuming dispatch at " + resetAt.UTC().Format(time.RFC3339) + "\n"
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

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit config warnings and run summaries as JSON")
	phaseFilter := fs.String("phase", "", "filter by comma-separated run phases")
	workflowFilter := fs.String("workflow", "", "filter by workflow name")
	limit := fs.Int("limit", 0, "maximum number of runs to show (default: all)")
	watch := fs.Bool("watch", false, "refresh the status board until interrupted")
	interval := fs.Duration("interval", defaultStatusWatchInterval, "watch refresh interval")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers status [--json] [--phase=<phase>[,<phase>...]] [--workflow=<name>] [--limit=N] [--watch [--interval=2s]] [path]\n\n"+
			"Validate active config, show warnings, and list runs under an instance's\n"+
			"runs/ directory with their current phase (default path \".\"). Exit codes:\n"+
			"0 = OK, 1 = validation errors, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *limit < 0 {
		pf(stderr, "error: --limit must be non-negative\n")
		return 2
	}
	if *interval <= 0 {
		pf(stderr, "error: --interval must be greater than zero\n")
		return 2
	}
	if *watch && *jsonOutput {
		pf(stderr, "error: --watch cannot be used with --json\n")
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
	if *watch && !statusOutputIsTerminal(stdout) {
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
	if _, err := instance.LoadConfig(l.ConfigFile()); err != nil {
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
	warnings := report.Warnings()
	if _, err := compiledMachines(set, goobersByName(set)); err != nil {
		printValidationWarnings(stderr, warnings)
		pf(stderr, "error: invalid workflow: %v\n", err)
		return 1
	}

	options := statusOptions{
		phases:   phases,
		workflow: *workflowFilter,
		limit:    *limit,
	}

	loadRuns := func() ([]runSummary, error) {
		return listRuns(l.RunsDir())
	}
	// #712: surface an active provider-quota pause — best-effort (a
	// missing/unreadable instance log just means no line, never a status
	// failure). A closure, not a one-shot read, so the watch loop below picks
	// up a newly-recorded (or newly-passed) pause on every redraw, the same
	// way loadRuns re-reads runs/ each tick.
	loadPauseLine := func() string {
		events, err := journal.ReadInstanceLog(l.SchedulerDir())
		if err != nil {
			return ""
		}
		return providerQuotaStatusLine(events, time.Now())
	}
	if *watch {
		// Config warnings are a static, one-time-per-invocation check (unlike
		// the provider-quota pause, which is live scheduler state) — printed
		// once before entering the redraw loop, not re-shown every tick.
		printValidationWarnings(stdout, warnings)
		ctx, stop := signals.SetupSignalContext()
		defer stop()
		if err := watchStatus(ctx, *interval, options, stdout, loadRuns, loadPauseLine); err != nil {
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
	if line := loadPauseLine(); line != "" {
		pf(stdout, "%s", line)
	}
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
		return filtered[i].StartedAt.Before(filtered[j].StartedAt)
	})
	if options.limit > 0 && len(filtered) > options.limit {
		filtered = filtered[len(filtered)-options.limit:]
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
	loadPauseLine func() string,
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
		renderStatusWatchFrame(stdout, loadPauseLine(), selectStatusRuns(allRuns, options), changedStatusRuns(previous, current))
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

func renderStatusWatchFrame(stdout io.Writer, pauseLine string, runs []runSummary, changed map[string]struct{}) {
	pf(stdout, statusClearScreen)
	if pauseLine != "" {
		pf(stdout, "%s", pauseLine)
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
