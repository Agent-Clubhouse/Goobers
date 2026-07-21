package main

import (
	"encoding/json"
	"flag"
	"io"
	"math"
	"os"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

type statsJSONSummary struct {
	Since                *time.Time              `json:"since,omitempty"`
	Runs                 statsJSONRuns           `json:"runs"`
	PullRequests         statsJSONPullRequests   `json:"pullRequests"`
	Issues               statsJSONIssues         `json:"issues"`
	BusiestWorkflow      *statsJSONWorkflow      `json:"busiestWorkflow"`
	AgenticStageDuration *statsJSONStageDuration `json:"agenticStageDuration"`
}

type statsJSONRuns struct {
	Total       int             `json:"total"`
	ByPhase     statsJSONPhases `json:"byPhase"`
	SuccessRate float64         `json:"successRate"`
}

type statsJSONPhases struct {
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Aborted   int `json:"aborted"`
	Escalated int `json:"escalated"`
	Running   int `json:"running"`
	Other     int `json:"other"`
}

type statsJSONPullRequests struct {
	Opened int `json:"opened"`
	Merged int `json:"merged"`
}

type statsJSONIssues struct {
	Claimed int `json:"claimed"`
	Closed  int `json:"closed"`
}

type statsJSONWorkflow struct {
	Name string `json:"name"`
	Runs int    `json:"runs"`
}

type statsJSONStageDuration struct {
	Attempts            int     `json:"attempts"`
	AverageMilliseconds float64 `json:"averageMilliseconds"`
	LongestMilliseconds int64   `json:"longestMilliseconds"`
	LongestStage        string  `json:"longestStage"`
	LongestWorkflow     string  `json:"longestWorkflow"`
	LongestRunID        string  `json:"longestRunId"`
}

const statsHelp = "Usage: goobers stats [--since <duration>] [--json] [path]\n\n" +
	"Show the instance's run outcomes, provider mutations, busiest workflow,\n" +
	"and agentic-stage durations (default path \".\"). Exit codes: 0 = OK,\n" +
	"2 = usage/IO error.\n"

func runStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinceDuration := fs.Duration("since", 0, "only include activity from the preceding duration")
	jsonOutput := fs.Bool("json", false, "emit the summary as JSON")
	fs.Usage = helpUsage(stderr, "stats")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	sinceSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "since" {
			sinceSet = true
		}
	})
	if sinceSet && *sinceDuration <= 0 {
		pf(stderr, "error: --since must be greater than zero\n")
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

	var since time.Time
	if sinceSet {
		since = time.Now().UTC().Add(-*sinceDuration)
	}
	db, err := openRollup(l, false)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	defer func() { _ = db.Close() }()

	summary, err := db.InstanceSummaryStats(since)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	view := newStatsJSONSummary(summary, since)
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(view); err != nil {
			pf(stderr, "error: encode stats: %v\n", err)
			return 2
		}
		return 0
	}
	if summary.TotalRuns == 0 {
		if !sinceSet {
			pln(stdout, "no runs yet — try goobers run <workflow>")
			return 0
		}
		lifetime, err := db.InstanceSummaryStats(time.Time{})
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		if lifetime.TotalRuns == 0 {
			pln(stdout, "no runs yet — try goobers run <workflow>")
			return 0
		}
		if !statsSummaryHasActivity(summary) {
			pf(stdout, "no runs in the last %s\n", sinceDuration.String())
			return 0
		}
	}

	if sinceSet {
		pf(stdout, "Goobers stats (last %s)\n", sinceDuration.String())
	} else {
		pln(stdout, "Goobers stats")
	}
	pf(stdout, "%-18s %d total (completed %d, failed %d, aborted %d, escalated %d, running %d",
		"Runs", summary.TotalRuns, summary.CompletedRuns, summary.FailedRuns,
		summary.AbortedRuns, summary.EscalatedRuns, summary.RunningRuns)
	if summary.OtherRuns > 0 {
		pf(stdout, ", other %d", summary.OtherRuns)
	}
	pln(stdout, ")")
	pf(stdout, "%-18s %.1f%%\n", "Success rate", summary.SuccessRate*100)
	pf(stdout, "%-18s %d opened, %d merged\n", "Pull requests", summary.PullRequestsOpened, summary.PullRequestsMerged)
	pf(stdout, "%-18s %d claimed, %d closed\n", "Issues", summary.IssuesClaimed, summary.IssuesClosed)
	if summary.BusiestWorkflow != "" {
		pf(stdout, "%-18s %s (%d runs)\n", "Busiest workflow", summary.BusiestWorkflow, summary.BusiestWorkflowRuns)
	} else {
		pf(stdout, "%-18s none\n", "Busiest workflow")
	}
	if summary.AgenticStageAttempts > 0 {
		pf(stdout, "%-18s %d attempts, %s average, %s longest (%s/%s)\n",
			"Agentic stages",
			summary.AgenticStageAttempts,
			formatStatsDuration(summary.AvgAgenticStageDurationMs),
			formatStatsDuration(float64(summary.LongestAgenticStageMs)),
			summary.LongestAgenticWorkflow,
			summary.LongestAgenticStage,
		)
	} else {
		pf(stdout, "%-18s no duration data\n", "Agentic stages")
	}
	return 0
}

func newStatsJSONSummary(summary rollup.InstanceSummary, since time.Time) statsJSONSummary {
	view := statsJSONSummary{
		Runs: statsJSONRuns{
			Total: summary.TotalRuns,
			ByPhase: statsJSONPhases{
				Completed: summary.CompletedRuns,
				Failed:    summary.FailedRuns,
				Aborted:   summary.AbortedRuns,
				Escalated: summary.EscalatedRuns,
				Running:   summary.RunningRuns,
				Other:     summary.OtherRuns,
			},
			SuccessRate: summary.SuccessRate,
		},
		PullRequests: statsJSONPullRequests{
			Opened: summary.PullRequestsOpened,
			Merged: summary.PullRequestsMerged,
		},
		Issues: statsJSONIssues{
			Claimed: summary.IssuesClaimed,
			Closed:  summary.IssuesClosed,
		},
	}
	if !since.IsZero() {
		view.Since = &since
	}
	if summary.BusiestWorkflow != "" {
		view.BusiestWorkflow = &statsJSONWorkflow{Name: summary.BusiestWorkflow, Runs: summary.BusiestWorkflowRuns}
	}
	if summary.AgenticStageAttempts > 0 {
		view.AgenticStageDuration = &statsJSONStageDuration{
			Attempts:            summary.AgenticStageAttempts,
			AverageMilliseconds: summary.AvgAgenticStageDurationMs,
			LongestMilliseconds: summary.LongestAgenticStageMs,
			LongestStage:        summary.LongestAgenticStage,
			LongestWorkflow:     summary.LongestAgenticWorkflow,
			LongestRunID:        summary.LongestAgenticRunID,
		}
	}
	return view
}

func formatStatsDuration(milliseconds float64) string {
	return (time.Duration(math.Round(milliseconds)) * time.Millisecond).String()
}

func statsSummaryHasActivity(summary rollup.InstanceSummary) bool {
	return summary.TotalRuns > 0 ||
		summary.PullRequestsOpened > 0 ||
		summary.PullRequestsMerged > 0 ||
		summary.IssuesClaimed > 0 ||
		summary.IssuesClosed > 0 ||
		summary.AgenticStageAttempts > 0
}
