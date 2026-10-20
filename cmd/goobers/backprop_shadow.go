package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

// withStatusBackpropOverrides resolves each summarized workflow's Backprop
// mode against the gaggle defaults it inherits (#7117, #7120).
func withStatusBackpropOverrides(load statusFleetSummaryLoader, gaggles []apiv1.Gaggle) statusFleetSummaryLoader {
	overrides := readservice.GaggleBackpropOverrides(gaggles)
	if len(overrides) == 0 {
		return load
	}
	return func(
		definitions []apiv1.Workflow,
		runs []runSummary,
		status readservice.SchedulerStatus,
		now time.Time,
	) (statusFleetSummary, error) {
		summary, err := load(definitions, runs, status, now)
		if err != nil {
			return summary, err
		}
		workflows := make(map[statusWorkflowKey]*apiv1.Workflow, len(definitions))
		for i := range definitions {
			workflows[statusWorkflowKey{gaggle: definitions[i].Spec.Gaggle, workflow: definitions[i].Name}] = &definitions[i]
		}
		for i := range summary.Workflows {
			workflow := &summary.Workflows[i]
			definition := workflows[statusWorkflowKey{gaggle: workflow.Gaggle, workflow: workflow.Workflow}]
			if definition == nil {
				continue
			}
			inherited := instance.InheritedGaggleBackprop(definition, overrides[workflow.Gaggle])
			workflow.Backprop = readservice.WorkflowBackpropFor(definition.Spec.Backprop, inherited)
		}
		return summary, nil
	}
}

const telemetryShadowHelp = "Usage: goobers telemetry shadow [--json] [--gaggle=name] [--workflow=name] [--since=RFC3339] [--until=RFC3339] [path]\n\n" +
	"Compare what Backprop's active fault audit would have filed and recommended\n" +
	"from shadow-mode attribution with what actually happened to those runs.\n" +
	"Shadow runs come from workflows with backprop.mode: shadow and from\n" +
	"workflows observed through a gaggle's backprop.mode: shadow override. This\n" +
	"report is read-only: it records no filing, cooldown, or verification state,\n" +
	"and no gate, filing pass, or default portal view reads it. Exit codes:\n" +
	"0 = OK, 1 = query error, 2 = usage/config error.\n"

func runTelemetryShadow(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("telemetry shadow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit the shadow comparison report as JSON")
	gaggle := fs.String("gaggle", "", "filter to one gaggle")
	workflow := fs.String("workflow", "", "filter to one workflow name")
	sinceValue := fs.String("since", "", "include runs at or after this RFC3339 timestamp")
	untilValue := fs.String("until", "", "include runs at or before this RFC3339 timestamp")
	fs.Usage = helpUsage(stderr, "telemetry shadow")
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
	var overrides map[string]*apiv1.GaggleBackprop
	if _, statErr := os.Stat(l.ConfigDir()); statErr == nil {
		set, report, loadErr := instance.LoadConfigDir(l.ConfigDir())
		if loadErr != nil {
			printValidationIssues(stderr, report)
			pf(stderr, "error: load config directory: %v\n", loadErr)
			return 2
		}
		printValidationWarnings(stderr, report.CLIWarnings())
		overrides = readservice.GaggleBackpropOverrides(set.Gaggles)
	} else if !os.IsNotExist(statErr) {
		pf(stderr, "error: inspect config directory: %v\n", statErr)
		return 2
	}
	reads, closeReads, err := openOptionalReadModel(l)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	defer closeReads()
	report, err := readservice.StoredShadowBackprop(context.Background(), root, reads, readservice.StoredAttributionQuery{
		Gaggle: *gaggle, Workflow: *workflow, Since: since, Until: until,
	}, overrides, creditgraph.FaultAuditConfig{})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			pf(stderr, "error: encode shadow report: %v\n", err)
			return 1
		}
		return 0
	}
	renderShadowBackpropReport(stdout, report)
	return 0
}

func renderShadowBackpropReport(stdout io.Writer, report readservice.ShadowBackpropReport) {
	if len(report.Runs) == 0 {
		pln(stdout, "no shadow-mode runs found")
		return
	}
	pf(stdout, "shadow runs: %d (report-only; nothing filed)\n", len(report.Runs))
	pf(stdout, "%-34s  %-20s  %-16s  %-10s  %s\n", "RUN ID", "WORKFLOW", "SOURCE", "PHASE", "ATTRIBUTION")
	for _, run := range report.Runs {
		pf(stdout, "%-34s  %-20s  %-16s  %-10s  %s (%d causes)\n", run.RunID, run.Workflow, run.Source, run.Phase, run.Status, run.Causes)
	}
	if len(report.Comparisons) == 0 {
		pln(stdout, "active mode would have filed no findings")
		return
	}
	pf(stdout, "active mode would have filed %d finding(s):\n", len(report.Comparisons))
	for _, comparison := range report.Comparisons {
		filed := "not filed"
		if comparison.ActuallyFiled {
			filed = "also filed by the active audit"
		}
		pf(stdout, "  %s  %s -> %s: %s\n", comparison.FindingID, comparison.Domain, comparison.RecommendedOwner, comparison.RecommendedAction)
		pf(stdout, "    runs=%d actual=%v; %s\n", len(comparison.RunIDs), comparison.ActualPhases, filed)
	}
}
