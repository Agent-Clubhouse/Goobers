package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
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

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit run summaries as JSON")
	phaseFilter := fs.String("phase", "", "filter by comma-separated run phases")
	workflowFilter := fs.String("workflow", "", "filter by workflow name")
	limit := fs.Int("limit", 0, "maximum number of runs to show (default: all)")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers status [--json] [--phase=<phase>[,<phase>...]] [--workflow=<name>] [--limit=N] [path]\n\n"+
			"List runs under an instance's runs/ directory with their current phase\n"+
			"(default path \".\"). Exit codes: 0 = OK, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *limit < 0 {
		pf(stderr, "error: --limit must be non-negative\n")
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
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}
	runs, err := listRuns(l.RunsDir())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	// #712: surface an active provider-quota pause before the run table —
	// best-effort (a missing/unreadable instance log just means no line,
	// never a status failure) and skipped in --json mode, since the JSON
	// output's shape is a run-summary array, not an object with room for a
	// side channel.
	if !*jsonOutput {
		if events, ierr := journal.ReadInstanceLog(l.SchedulerDir()); ierr == nil {
			if line := providerQuotaStatusLine(events, time.Now()); line != "" {
				pf(stdout, "%s", line)
			}
		}
	}
	if len(phases) > 0 || *workflowFilter != "" {
		filtered := runs[:0]
		for _, run := range runs {
			if *workflowFilter != "" && run.Workflow != *workflowFilter {
				continue
			}
			if len(phases) > 0 {
				if _, ok := phases[run.Phase]; !ok {
					continue
				}
			}
			filtered = append(filtered, run)
		}
		runs = filtered
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.Before(runs[j].StartedAt) })
	if *limit > 0 && len(runs) > *limit {
		runs = runs[len(runs)-*limit:]
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(statusJSONSummaries(runs)); err != nil {
			pf(stderr, "error: encode status: %v\n", err)
			return 2
		}
		return 0
	}
	if len(runs) == 0 {
		pln(stdout, "no runs found — trigger one with 'goobers run <workflow>'")
		return 0
	}

	pf(stdout, "%-34s  %-24s  %-10s  %-10s  %s\n", "RUN ID", "WORKFLOW", "GAGGLE", "PHASE", "STARTED")
	for _, r := range runs {
		pf(stdout, "%-34s  %-24s  %-10s  %-10s  %s\n",
			r.RunID, r.Workflow, r.Gaggle, r.Phase, r.StartedAt.Format(time.RFC3339))
	}
	return 0
}
