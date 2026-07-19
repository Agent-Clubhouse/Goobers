package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
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
		if err := rollup.Rebuild(l.TelemetryDB(), l.RunsDir(), l.SchedulerDir()); err != nil {
			return nil, err
		}
	}
	return rollup.Open(l.TelemetryDB())
}

func runTelemetryStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("telemetry stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit telemetry statistics as JSON")
	workflow := fs.String("workflow", "", "filter to one workflow name")
	gaggle := fs.String("gaggle", "", "filter to one gaggle")
	sinceValue := fs.String("since", "", "include runs started at or after this RFC3339 timestamp")
	untilValue := fs.String("until", "", "include runs started at or before this RFC3339 timestamp")
	rebuild := fs.Bool("rebuild", false, "force a full rebuild from run journals before querying (only needed for runs journaled out-of-band, e.g. hand-repaired or pre-#126)")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers telemetry stats [--json] [--workflow=name] [--gaggle=name] [--since=RFC3339] [--until=RFC3339] [--rebuild] [path]\n\n"+
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
	since, until, err := parseTelemetryWindow(*sinceValue, *untilValue)
	if err != nil {
		pf(stderr, "error: %v\n", err)
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
	queries, err := readservice.NewTelemetry(db)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	result, err := queries.TelemetryStats(context.Background(), readservice.TelemetryStatsRequest{
		Workflow: *workflow,
		Gaggle:   *gaggle,
		Since:    since,
		Until:    until,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			pf(stderr, "error: encode telemetry stats: %v\n", err)
			return 2
		}
		return 0
	}
	if len(result.Runs) == 0 {
		pln(stdout, "no runs found")
		return 0
	}
	pln(stdout, "WORKFLOW STATS")
	pf(stdout, "%-24s  %6s  %9s  %6s  %6s  %8s  %8s  %8s  %8s\n",
		"WORKFLOW", "TOTAL", "COMPLETED", "FAILED", "OTHER", "SUCCESS%", "AVG(ms)", "MIN(ms)", "MAX(ms)")
	for _, r := range result.Runs {
		pf(stdout, "%-24s  %6d  %9d  %6d  %6d  %8s  %8s  %8s  %8s\n",
			r.Workflow, r.TotalRuns, r.CompletedRuns, r.FailedRuns, r.OtherRuns,
			formatTelemetryRate(r.SuccessRate), formatTelemetryFloat(r.AvgDurationMs),
			formatTelemetryInt(r.MinDurationMs), formatTelemetryInt(r.MaxDurationMs))
	}

	pln(stdout, "\nSTAGE STATS")
	pf(stdout, "%-16s  %9s  %9s  %6s  %8s  %8s  %8s  %8s\n",
		"STAGE", "ATTEMPTS", "SUCCEEDED", "FAILED", "SUCCESS%", "AVG(ms)", "MIN(ms)", "MAX(ms)")
	for _, s := range result.Stages {
		pf(stdout, "%-16s  %9d  %9d  %6d  %8s  %8s  %8s  %8s\n",
			s.Stage, s.TotalAttempts, s.SucceededAttempts, s.FailedAttempts,
			formatTelemetryRate(s.SuccessRate), formatTelemetryFloat(s.AvgDurationMs),
			formatTelemetryInt(s.MinDurationMs), formatTelemetryInt(s.MaxDurationMs))
	}
	return 0
}

func runTelemetryErrors(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("telemetry errors", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit recent errors as JSON")
	workflow := fs.String("workflow", "", "filter to one workflow name")
	gaggle := fs.String("gaggle", "", "filter to one gaggle")
	class := fs.String("class", "", "filter to one error class")
	limit := fs.Int("limit", 50, "max errors to show (newest first)")
	sinceValue := fs.String("since", "", "include errors at or after this RFC3339 timestamp")
	untilValue := fs.String("until", "", "include errors at or before this RFC3339 timestamp")
	rebuild := fs.Bool("rebuild", false, "force a full rebuild from run journals before querying (only needed for runs journaled out-of-band, e.g. hand-repaired or pre-#126)")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers telemetry errors [--json] [--workflow=name] [--gaggle=name] [--class=name] [--since=RFC3339] [--until=RFC3339] [--limit=N] [--rebuild] [path]\n\n"+
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
	since, until, err := parseTelemetryWindow(*sinceValue, *untilValue)
	if err != nil {
		pf(stderr, "error: %v\n", err)
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
	queries, err := readservice.NewTelemetry(db)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	result, err := queries.TelemetryErrors(context.Background(), readservice.TelemetryErrorsRequest{
		Workflow:   *workflow,
		Gaggle:     *gaggle,
		ErrorClass: *class,
		Since:      since,
		Until:      until,
		Limit:      *limit,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(result.Items); err != nil {
			pf(stderr, "error: encode telemetry errors: %v\n", err)
			return 2
		}
		return 0
	}
	if len(result.Items) == 0 {
		pln(stdout, "no errors found")
		return 0
	}
	pf(stdout, "%-34s  %-20s  %-12s  %-24s  %-7s  %s\n", "RUN ID", "WORKFLOW", "STAGE", "CODE", "CLASS", "OCCURRED")
	for _, e := range result.Items {
		pf(stdout, "%-34s  %-20s  %-12s  %-24s  %-7s  %s\n",
			e.RunID, e.Workflow, e.Stage, e.Code, e.ErrorClass, e.OccurredAt.Format(time.RFC3339))
		if e.Message != "" {
			pf(stdout, "  %s\n", e.Message)
		}
	}
	return 0
}

func parseTelemetryWindow(sinceValue, untilValue string) (time.Time, time.Time, error) {
	parse := func(name, value string) (time.Time, error) {
		if value == "" {
			return time.Time{}, nil
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, fmt.Errorf("--%s must be an RFC3339 timestamp", name)
		}
		return parsed, nil
	}
	since, err := parse("since", sinceValue)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	until, err := parse("until", untilValue)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return time.Time{}, time.Time{}, fmt.Errorf("--since must not be after --until")
	}
	return since, until, nil
}

func formatTelemetryRate(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1f%%", *value*100)
}

func formatTelemetryFloat(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.0f", *value)
}

func formatTelemetryInt(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *value)
}
