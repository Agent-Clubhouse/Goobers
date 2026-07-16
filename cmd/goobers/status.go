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
)

type statusJSONSummary struct {
	RunID     string    `json:"runId"`
	Workflow  string    `json:"workflow"`
	Gaggle    string    `json:"gaggle"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"startedAt"`
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
		if err := json.NewEncoder(stdout).Encode(summaries); err != nil {
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
