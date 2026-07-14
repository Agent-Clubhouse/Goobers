package main

import (
	"flag"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func runTelemetry(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		pf(w, "Usage: goobers telemetry <stats|errors> [flags] [path]\n\n"+
			"stats:  success rate / durations per workflow + stage\n"+
			"errors: recent errors across runs, by class, with run/stage refs\n")
	}
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "stats":
		return runTelemetryStats(args[1:], stdout, stderr)
	case "errors":
		return runTelemetryErrors(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		pf(stderr, "goobers telemetry: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

// openRollup opens the telemetry rollup, trusting the incremental ingest
// `goobers up`/`run` already do on every run finish (issue #127) unless
// rebuild is set. It used to unconditionally call rollup.Rebuild — which
// os.Removes the shared telemetry.db — on every single query; two concurrent
// CLI invocations (e.g. `goobers trace` racing `goobers telemetry stats`)
// could unlink each other mid-ingest, and every query paid an O(all-runs-
// ever) full rescan just to stay correct. Now a query only pays that cost
// when explicitly asked (--rebuild), e.g. to pick up runs journaled
// out-of-band (a hand-repaired run dir, or an instance upgraded from a
// pre-#126 binary that never incrementally ingested).
func openRollup(l instance.Layout, rebuild bool) (*rollup.DB, error) {
	if rebuild {
		if err := rollup.Rebuild(l.TelemetryDB(), l.RunsDir()); err != nil {
			return nil, err
		}
	}
	return rollup.Open(l.TelemetryDB())
}

func runTelemetryStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("telemetry stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workflow := fs.String("workflow", "", "filter to one workflow name")
	rebuild := fs.Bool("rebuild", false, "force a full rebuild from run journals before querying (only needed for runs journaled out-of-band, e.g. hand-repaired or pre-#126)")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers telemetry stats [--workflow=name] [--rebuild] [path]\n\n"+
			"Success rate and duration aggregates per workflow and per stage,\n"+
			"across every run (default path \".\"). Exit codes: 0 = OK, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
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
	db, err := openRollup(l, *rebuild)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	defer func() { _ = db.Close() }()

	result, err := db.Stats(rollup.StatsRequest{Workflow: *workflow})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	if len(result.Runs) == 0 {
		pln(stdout, "no runs found")
		return 0
	}
	pln(stdout, "WORKFLOW STATS")
	pf(stdout, "%-24s  %6s  %9s  %6s  %6s  %8s  %8s  %8s  %8s\n",
		"WORKFLOW", "TOTAL", "COMPLETED", "FAILED", "OTHER", "SUCCESS%", "AVG(ms)", "MIN(ms)", "MAX(ms)")
	for _, r := range result.Runs {
		pf(stdout, "%-24s  %6d  %9d  %6d  %6d  %7.1f%%  %8.0f  %8d  %8d\n",
			r.Workflow, r.TotalRuns, r.CompletedRuns, r.FailedRuns, r.OtherRuns,
			r.SuccessRate*100, r.AvgDurationMs, r.MinDurationMs, r.MaxDurationMs)
	}

	pln(stdout, "\nSTAGE STATS")
	pf(stdout, "%-16s  %9s  %9s  %6s  %8s  %8s  %8s  %8s\n",
		"STAGE", "ATTEMPTS", "SUCCEEDED", "FAILED", "SUCCESS%", "AVG(ms)", "MIN(ms)", "MAX(ms)")
	for _, s := range result.Stages {
		pf(stdout, "%-16s  %9d  %9d  %6d  %7.1f%%  %8.0f  %8d  %8d\n",
			s.Stage, s.TotalAttempts, s.SucceededAttempts, s.FailedAttempts,
			s.SuccessRate*100, s.AvgDurationMs, s.MinDurationMs, s.MaxDurationMs)
	}
	return 0
}

func runTelemetryErrors(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("telemetry errors", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workflow := fs.String("workflow", "", "filter to one workflow name")
	class := fs.String("class", "", "filter to one error class")
	limit := fs.Int("limit", 50, "max errors to show (newest first)")
	rebuild := fs.Bool("rebuild", false, "force a full rebuild from run journals before querying (only needed for runs journaled out-of-band, e.g. hand-repaired or pre-#126)")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers telemetry errors [--workflow=name] [--class=name] [--limit=N] [--rebuild] [path]\n\n"+
			"Recent errors across every run, newest first, with run/stage refs\n"+
			"(default path \".\"). Exit codes: 0 = OK, 2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
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
	db, err := openRollup(l, *rebuild)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	defer func() { _ = db.Close() }()

	errs, err := db.Errors(rollup.ErrorsRequest{Workflow: *workflow, ErrorClass: *class, Limit: *limit})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if len(errs) == 0 {
		pln(stdout, "no errors found")
		return 0
	}
	pf(stdout, "%-34s  %-20s  %-12s  %-24s  %-7s  %s\n", "RUN ID", "WORKFLOW", "STAGE", "CODE", "CLASS", "OCCURRED")
	for _, e := range errs {
		pf(stdout, "%-34s  %-20s  %-12s  %-24s  %-7s  %s\n",
			e.RunID, e.Workflow, e.Stage, e.Code, e.ErrorClass, e.OccurredAt.Format(time.RFC3339))
		if e.Message != "" {
			pf(stdout, "  %s\n", e.Message)
		}
	}
	return 0
}
