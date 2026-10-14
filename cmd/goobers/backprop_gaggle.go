package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

const telemetryGaggleHelp = "Usage: goobers telemetry gaggle --gaggle=name [--json] [--since=RFC3339] [--until=RFC3339] [path]\n\n" +
	"Summarize one gaggle's active Backprop attribution across its workflows:\n" +
	"per-workflow and per-stage credit and blame share, the top recurring\n" +
	"findings, each workflow's trend against the previous window of the same\n" +
	"length, and cross-workflow links. A link exists only where two runs\n" +
	"recorded the same identifier (an external reference touched by both, or a\n" +
	"run continuation); nothing is inferred. The window defaults to the seven\n" +
	"days before --until (default now). Shadow records are never read. Exit\n" +
	"codes: 0 = OK, 1 = query error, 2 = usage error.\n"

func runTelemetryGaggle(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("telemetry gaggle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit the gaggle report as JSON")
	gaggle := fs.String("gaggle", "", "gaggle to report on (required)")
	sinceValue := fs.String("since", "", "include runs started at or after this RFC3339 timestamp")
	untilValue := fs.String("until", "", "include runs started before this RFC3339 timestamp")
	fs.Usage = helpUsage(stderr, "telemetry gaggle")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || strings.TrimSpace(*gaggle) == "" {
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
	reads, closeReads, err := openOptionalReadModel(instance.NewLayout(root))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer closeReads()
	report, err := readservice.StoredGaggleBackprop(context.Background(), root, reads, readservice.GaggleBackpropQuery{
		Gaggle: *gaggle, Since: since, Until: until,
	}, time.Now())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			pf(stderr, "error: encode gaggle report: %v\n", err)
			return 1
		}
		return 0
	}
	renderGaggleBackpropReport(stdout, report)
	return 0
}

// openOptionalReadModel opens the run read model when it exists. A missing
// read model is not an error: reports over it are simply empty.
func openOptionalReadModel(l instance.Layout) (readmodel.Reader, func(), error) {
	if _, err := os.Stat(l.ReadDB()); os.IsNotExist(err) {
		return nil, func() {}, nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("inspect run read model %s: %w", l.ReadDB(), err)
	}
	store, err := readmodel.Open(l.ReadDB())
	if err != nil {
		return nil, nil, fmt.Errorf("open run read model %s: %w", l.ReadDB(), err)
	}
	return store, func() { _ = store.Close() }, nil
}

func renderGaggleBackpropReport(stdout io.Writer, report readservice.GaggleBackpropReport) {
	pf(stdout, "gaggle %s: %d active Backprop run(s) %s .. %s\n", report.Gaggle, report.Runs,
		report.Window.Since.Format(time.RFC3339), report.Window.Until.Format(time.RFC3339))
	if report.Truncated {
		pln(stdout, "note: the run limit or scan budget was reached; narrow --since/--until for a complete window")
	}
	if report.Runs == 0 {
		return
	}
	pf(stdout, "%-24s  %5s  %6s  %7s  %7s  %10s\n", "WORKFLOW", "RUNS", "FAILED", "CREDIT", "BLAME", "DOWNSTREAM")
	for _, workflow := range report.Workflows {
		pf(stdout, "%-24s  %5d  %6d  %7.2f  %7.2f  %10d\n", workflow.Workflow, workflow.Runs, workflow.Failed,
			workflow.CreditShare, workflow.BlameShare, workflow.DownstreamFailures)
	}
	pln(stdout, "stages:")
	for _, stage := range report.Stages {
		pf(stdout, "  %s/%s  credit=%.2f blame=%.2f runs=%d\n", stage.Workflow, stage.Stage, stage.CreditShare, stage.BlameShare, stage.Runs)
	}
	pln(stdout, "recurring findings:")
	for _, finding := range report.RecurringFindings {
		pf(stdout, "  %s/%s  %s  runs=%d\n", finding.Workflow, finding.Stage, finding.Class, finding.Runs)
	}
	pln(stdout, "trend vs previous window:")
	for _, trend := range report.Trends {
		pf(stdout, "  %s  runs %d -> %d  failure rate %.2f -> %.2f  blame %.2f -> %.2f\n", trend.Workflow,
			trend.PreviousRuns, trend.Runs, trend.PreviousFailureRate, trend.FailureRate, trend.PreviousBlameShare, trend.BlameShare)
	}
	pln(stdout, "cross-workflow links:")
	for _, link := range report.Links {
		pf(stdout, "  %s (%s) -> %s (%s, %s) via %s %s\n", link.FromRunID, link.FromWorkflow,
			link.ToRunID, link.ToWorkflow, link.ToPhase, link.Basis, link.Identifier)
	}
}
