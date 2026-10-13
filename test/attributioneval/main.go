// Command attributioneval measures Backprop attribution accuracy on a
// versioned, labeled eval set (#7123).
//
// It builds and attributes every case's journal, scores the most confident
// cause against the case's label at each granularity (domain, workflow,
// stage, step, class), reports confidence calibration, and fails when any
// accuracy drops below the recorded baseline. Run from the repository root:
//
//	go run ./test/attributioneval            # check against the baseline
//	go run ./test/attributioneval -report=r.json
//	go run ./test/attributioneval -update    # record a new baseline
//	go run ./test/attributioneval -publish=<instance-root>
//
// -publish records the report's summary on a Goobers instance after the
// baseline check passes: telemetry read surfaces (and so the portal) report it,
// and the fault auditor derives its filing confidence threshold from its
// calibration.
//
// The same guard runs in CI as TestAttributionEvalRegressionGuard.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/readservice"
)

var (
	defaultSetPath      = filepath.Join("test", "attributioneval", "testdata", "v1.json")
	defaultBaselinePath = filepath.Join("test", "attributioneval", "testdata", "v1.baseline.json")
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("attributioneval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	setPath := flags.String("set", defaultSetPath, "labeled attribution eval set")
	baselinePath := flags.String("baseline", defaultBaselinePath, "recorded accuracy baseline")
	reportPath := flags.String("report", "", "write the full JSON report to this path")
	update := flags.Bool("update", false, "record this run's accuracy as the new baseline")
	publishRoot := flags.String("publish", "", "publish the report summary to this Goobers instance root")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *update && *publishRoot != "" {
		_, _ = fmt.Fprintln(stderr, "attributioneval: -publish requires a baseline check and cannot be combined with -update")
		return 2
	}
	report, err := evaluate(*setPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "attributioneval: %v\n", err)
		return 1
	}
	printSummary(stdout, report)
	if *reportPath != "" {
		if err := writeJSON(*reportPath, report); err != nil {
			_, _ = fmt.Fprintf(stderr, "attributioneval: write report: %v\n", err)
			return 1
		}
	}
	if *update {
		if err := writeJSON(*baselinePath, report.baseline()); err != nil {
			_, _ = fmt.Fprintf(stderr, "attributioneval: write baseline: %v\n", err)
			return 1
		}
		return 0
	}
	baseline, err := loadBaseline(*baselinePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "attributioneval: %v\n", err)
		return 1
	}
	if regressions := report.regressions(baseline); len(regressions) > 0 {
		for _, regression := range regressions {
			_, _ = fmt.Fprintf(stderr, "attributioneval: regression: %s\n", regression)
		}
		return 1
	}
	if *publishRoot != "" {
		if err := readservice.PublishAttributionEval(*publishRoot, report.summary()); err != nil {
			_, _ = fmt.Fprintf(stderr, "attributioneval: %v\n", err)
			return 1
		}
	}
	return 0
}

func evaluate(setPath string) (evalReport, error) {
	set, err := loadEvalSet(setPath)
	if err != nil {
		return evalReport{}, err
	}
	return runEval(set)
}

func printSummary(out io.Writer, report evalReport) {
	_, _ = fmt.Fprintf(out, "attribution eval %s (%d cases, %d unattributed, %s)\n",
		report.EvalSetVersion, report.CaseCount, report.Unattributed, report.EvalSetDigest)
	for _, entry := range report.Accuracy {
		_, _ = fmt.Fprintf(out, "  %-8s %d/%d = %.4f\n", entry.Granularity, entry.Correct, entry.Total, entry.Accuracy)
	}
	_, _ = fmt.Fprintf(out, "  calibration vs %s: brier=%.4f ece=%.4f\n", report.Calibration.Target,
		report.Calibration.BrierScore, report.Calibration.ExpectedCalibrationError)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
