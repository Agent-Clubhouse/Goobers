package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const telemetryHelp = "Usage: goobers telemetry <stats|errors|export|prune> [flags] [path]\n\n" +
	"stats:  success rate / durations per workflow + stage\n" +
	"errors: recent errors across runs, by class, with run/stage refs\n" +
	"export: re-emit a span-start-time window from journaled OTLP/JSON\n" +
	"prune:  remove terminal runs outside the configured retention bounds\n"

func runTelemetry(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) { pf(w, "%s", telemetryHelp) }
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

const telemetryPruneHelp = "Usage: goobers telemetry prune [--dry-run] [path]\n\n" +
	"Remove terminal run journals and all of their SQLite rollup rows when either\n" +
	"telemetry.retention.window or telemetry.retention.maxRuns is exceeded. Active\n" +
	"and paused runs are never removed. The configured 90d/500-run defaults apply\n" +
	"when a bound is omitted. This explicit command works even when automatic\n" +
	"retention is disabled. Exit codes: 0 = OK, 1 = prune error, 2 = usage/config error.\n"

func runTelemetryPrune(args []string, stdout, stderr io.Writer) int {
	return runTelemetryPruneAt(args, stdout, stderr, time.Now())
}

func runTelemetryPruneAt(args []string, stdout, stderr io.Writer, now time.Time) int {
	fs := flag.NewFlagSet("telemetry prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "report eligible terminal runs without deleting them")
	fs.Usage = helpUsage(stderr, "telemetry prune")
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

	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	retentionConfig := instance.TelemetryRetentionConfig{}
	if config.Telemetry.Retention != nil {
		retentionConfig = *config.Telemetry.Retention
	}
	results, err := pruneTelemetryRetention(layout, retentionConfig, nil, now, *dryRun)
	if err != nil {
		pf(stderr, "error: prune telemetry: %v\n", err)
		return 1
	}
	if len(results) == 0 {
		pln(stdout, "no telemetry runs eligible for pruning")
		return 0
	}
	verb := "pruned"
	if *dryRun {
		verb = "would prune"
	}
	for _, result := range results {
		pf(stdout, "%s run=%q reason=%s\n", verb, result.RunID, result.Reason)
	}
	return 0
}

const telemetryExportHelp = "Usage: goobers telemetry export --since=RFC3339 [--until=RFC3339] [path]\n\n" +
	"Re-emit journaled OTLP/JSON trace requests to stdout. --since is inclusive;\n" +
	"--until is exclusive when set. Window membership uses each span's start time.\n" +
	"Every discovered run journal is validated before output; missing, corrupt, or\n" +
	"unsupported OTLP data emits nothing and exits non-zero. Exit codes: 0 = OK,\n" +
	"1 = journal data error, 2 = usage/output error.\n"

func runTelemetryExport(args []string, stdout, stderr io.Writer) int {
	return runTelemetryExportWithExporter(args, stdout, stderr, telemetry.ExportJournalOTLP)
}

func runTelemetryExportWithExporter(
	args []string,
	stdout, stderr io.Writer,
	export func([]string, time.Time, time.Time, io.Writer) error,
) int {
	fs := flag.NewFlagSet("telemetry export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinceValue := fs.String("since", "", "inclusive RFC3339 span-start lower bound (required)")
	untilValue := fs.String("until", "", "exclusive RFC3339 span-start upper bound")
	fs.Usage = helpUsage(stderr, "telemetry export")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *sinceValue == "" {
		pf(stderr, "error: --since is required\n")
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

	layout := instance.NewLayout(root)
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", layout.ConfigFile())
		return 2
	}
	runDirs, err := layout.RunDirs()
	if err != nil {
		pf(stderr, "error: discover run journals: %v\n", err)
		return 2
	}
	staged, err := os.CreateTemp("", "goobers-telemetry-export-*")
	if err != nil {
		pf(stderr, "error: stage telemetry export: %v\n", err)
		return 2
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	if err := export(runDirs, since, until, staged); err != nil {
		pf(stderr, "error: export journaled OTLP: %v\n", err)
		var outputErr *telemetry.ExportOutputError
		if errors.As(err, &outputErr) {
			return 2
		}
		return 1
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		pf(stderr, "error: read staged telemetry export: %v\n", err)
		return 2
	}
	if _, err := io.Copy(stdout, staged); err != nil {
		pf(stderr, "error: write telemetry export: %v\n", err)
		return 2
	}
	return 0
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
		runDirs, err := l.RunDirs()
		if err != nil {
			return nil, err
		}
		if err := rollup.RebuildAll(l.TelemetryDB(), runDirs, l.SchedulerDir()); err != nil {
			return nil, err
		}
	}
	return rollup.Open(l.TelemetryDB())
}

const telemetryStatsHelp = "Usage: goobers telemetry stats [--json] [--workflow=name] [--gaggle=name] [--model=id] [--harness-version=version] [--group-by=model|harness-version]... [--since=RFC3339] [--until=RFC3339] [--rebuild] [path]\n\n" +
	"Success rate and duration aggregates per workflow and per stage,\n" +
	"across every run (default path \".\"). Agent filters retain matching agentic\n" +
	"stage attempts; a run that used multiple grouped cohorts appears in each.\n" +
	"Exit codes: 0 = OK, 2 = usage/IO error.\n"

type telemetryGroupBy struct {
	model          bool
	harnessVersion bool
}

func (g *telemetryGroupBy) String() string {
	var values []string
	if g.model {
		values = append(values, "model")
	}
	if g.harnessVersion {
		values = append(values, "harness-version")
	}
	return strings.Join(values, ",")
}

func (g *telemetryGroupBy) Set(value string) error {
	switch value {
	case "model":
		g.model = true
	case "harness-version":
		g.harnessVersion = true
	default:
		return fmt.Errorf("unknown group dimension %q (allowed: model, harness-version)", value)
	}
	return nil
}

func runTelemetryStats(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("telemetry stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit telemetry statistics as JSON")
	workflow := fs.String("workflow", "", "filter to one workflow name")
	gaggle := fs.String("gaggle", "", "filter to one gaggle")
	model := fs.String("model", "", "filter to one model id")
	harnessVersion := fs.String("harness-version", "", "filter to one harness version")
	var groupBy telemetryGroupBy
	fs.Var(&groupBy, "group-by", "group by model or harness-version; repeat to group by both")
	sinceValue := fs.String("since", "", "include runs started at or after this RFC3339 timestamp")
	untilValue := fs.String("until", "", "include runs started at or before this RFC3339 timestamp")
	rebuild := fs.Bool("rebuild", false, "force a full rebuild from run journals before querying (only needed for runs journaled out-of-band, e.g. hand-repaired or pre-#126)")
	fs.Usage = helpUsage(stderr, "telemetry stats")
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
		Workflow:              *workflow,
		Gaggle:                *gaggle,
		Model:                 *model,
		HarnessVersion:        *harnessVersion,
		GroupByModel:          groupBy.model,
		GroupByHarnessVersion: groupBy.harnessVersion,
		Since:                 since,
		Until:                 until,
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
	pf(stdout, "%-16s  %-24s  ", "GAGGLE", "WORKFLOW")
	writeTelemetryCohortColumns(stdout, groupBy, "MODEL", "HARNESS VERSION")
	pf(stdout, "%6s  %9s  %6s  %6s  %8s  %8s  %8s  %8s\n",
		"TOTAL", "COMPLETED", "FAILED", "OTHER", "SUCCESS%", "AVG(ms)", "MIN(ms)", "MAX(ms)")
	for _, r := range result.Runs {
		pf(stdout, "%-16s  %-24s  ", r.Gaggle, r.Workflow)
		writeTelemetryCohortColumns(stdout, groupBy, r.Model, r.HarnessVersion)
		pf(stdout, "%6d  %9d  %6d  %6d  %8s  %8s  %8s  %8s\n",
			r.TotalRuns, r.CompletedRuns, r.FailedRuns, r.OtherRuns,
			formatTelemetryRate(r.SuccessRate), formatTelemetryFloat(r.AvgDurationMs),
			formatTelemetryInt(r.MinDurationMs), formatTelemetryInt(r.MaxDurationMs))
	}

	pln(stdout, "\nSTAGE STATS")
	pf(stdout, "%-16s  %-24s  %-16s  ", "GAGGLE", "WORKFLOW", "STAGE")
	writeTelemetryCohortColumns(stdout, groupBy, "MODEL", "HARNESS VERSION")
	pf(stdout, "%9s  %9s  %6s  %8s  %8s  %8s  %8s\n",
		"ATTEMPTS", "SUCCEEDED", "FAILED", "SUCCESS%", "AVG(ms)", "MIN(ms)", "MAX(ms)")
	for _, s := range result.Stages {
		pf(stdout, "%-16s  %-24s  %-16s  ", s.Gaggle, s.Workflow, s.Stage)
		writeTelemetryCohortColumns(stdout, groupBy, s.Model, s.HarnessVersion)
		pf(stdout, "%9d  %9d  %6d  %8s  %8s  %8s  %8s\n",
			s.TotalAttempts, s.SucceededAttempts, s.FailedAttempts,
			formatTelemetryRate(s.SuccessRate), formatTelemetryFloat(s.AvgDurationMs),
			formatTelemetryInt(s.MinDurationMs), formatTelemetryInt(s.MaxDurationMs))
	}
	return 0
}

// writeTelemetryCohortColumns prints the optional model/harness-version cohort
// columns after the fixed gaggle/workflow(/stage) identity columns, in the
// same call for both header labels and data rows (formatTelemetryDimension is
// a no-op passthrough for the non-empty literal header labels).
func writeTelemetryCohortColumns(w io.Writer, groupBy telemetryGroupBy, model, harnessVersion string) {
	if groupBy.model {
		pf(w, "%-24s  ", formatTelemetryDimension(model))
	}
	if groupBy.harnessVersion {
		pf(w, "%-24s  ", formatTelemetryDimension(harnessVersion))
	}
}

func formatTelemetryDimension(value string) string {
	if value == "" {
		return "(unspecified)"
	}
	return value
}

const telemetryErrorsHelp = "Usage: goobers telemetry errors [--json] [--workflow=name] [--gaggle=name] [--class=name] [--since=RFC3339] [--until=RFC3339] [--limit=N] [--rebuild] [path]\n\n" +
	"Recent errors across every run, newest first, with run/stage refs\n" +
	"(default path \".\"). Exit codes: 0 = OK, 2 = usage/IO error.\n"

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
	fs.Usage = helpUsage(stderr, "telemetry errors")
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
