package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const candidateFindingsSchemaVersion = "goobers.dev/candidate-findings/v1"

type candidateFindingsArtifact struct {
	Schema   string           `json:"schema"`
	Window   string           `json:"window"`
	Since    time.Time        `json:"since"`
	Findings []rollup.Finding `json:"findings"`
	NoWork   bool             `json:"noWork,omitempty"`
	Note     string           `json:"note,omitempty"`
}

const (
	telemetryQueryNoRollupNote    = "no telemetry rollup yet"
	telemetryQueryNoFindingsNote  = "telemetry rollup has no candidate findings in the requested window"
	telemetryQueryCandidateFormat = "candidate-findings"
)

type telemetryAggregate string

const (
	telemetryAggregateAll              telemetryAggregate = "all"
	telemetryAggregateStageFailureRate telemetryAggregate = "stage-failure-rate"
	telemetryAggregateErrorSignature   telemetryAggregate = "error-signature"
	telemetryAggregateGateNoise        telemetryAggregate = "gate-noise"
)

type telemetryAggregateValues []telemetryAggregate

func (v *telemetryAggregateValues) String() string {
	values := make([]string, len(*v))
	for i, aggregate := range *v {
		values[i] = string(aggregate)
	}
	return strings.Join(values, ",")
}

func (v *telemetryAggregateValues) Set(raw string) error {
	var aggregate telemetryAggregate
	switch raw {
	case string(telemetryAggregateAll):
		aggregate = telemetryAggregateAll
	case string(telemetryAggregateStageFailureRate), "failure-rate":
		aggregate = telemetryAggregateStageFailureRate
	case string(telemetryAggregateErrorSignature), "error-signatures":
		aggregate = telemetryAggregateErrorSignature
	case string(telemetryAggregateGateNoise):
		aggregate = telemetryAggregateGateNoise
	default:
		return fmt.Errorf("unknown aggregate %q (allowed: all, stage-failure-rate, error-signature, gate-noise)", raw)
	}
	for _, existing := range *v {
		if existing == aggregate {
			return nil
		}
	}
	*v = append(*v, aggregate)
	return nil
}

func (v telemetryAggregateValues) includes(kind rollup.FindingKind) bool {
	if len(v) == 0 {
		return true
	}
	for _, aggregate := range v {
		switch aggregate {
		case telemetryAggregateAll:
			return true
		case telemetryAggregateStageFailureRate:
			if kind == rollup.FindingStageFailureRate {
				return true
			}
		case telemetryAggregateErrorSignature:
			if kind == rollup.FindingErrorSignature {
				return true
			}
		case telemetryAggregateGateNoise:
			if kind == rollup.FindingGateNeverFails || kind == rollup.FindingGateRepassChurn {
				return true
			}
		}
	}
	return false
}

type telemetryThresholdValue struct {
	thresholds *rollup.Thresholds
}

func (v *telemetryThresholdValue) String() string {
	return ""
}

func (v *telemetryThresholdValue) Set(raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return fmt.Errorf("threshold must be k=v, got %q", raw)
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	parsePositiveInt := func() (int, error) {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return 0, fmt.Errorf("%s must be a positive integer", key)
		}
		return n, nil
	}
	parseRate := func() (float64, error) {
		rate, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
			return 0, fmt.Errorf("%s must be a number between 0 and 1", key)
		}
		return rate, nil
	}

	switch key {
	case "min-samples", "minSamples":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MinSamples = n
	case "max-failure-rate", "maxFailureRate":
		rate, err := parseRate()
		if err != nil {
			return err
		}
		v.thresholds.MaxFailureRate = rate
	case "min-error-signature-count", "minErrorSignatureCount":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MinErrorSignatureCount = n
	case "min-gate-evaluations", "minGateEvaluations":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MinGateEvaluations = n
	case "max-gate-escalation-rate", "maxGateEscalationRate":
		rate, err := parseRate()
		if err != nil {
			return err
		}
		v.thresholds.MaxGateEscalationRate = rate
	case "max-flagged-runs", "maxFlaggedRuns":
		n, err := parsePositiveInt()
		if err != nil {
			return err
		}
		v.thresholds.MaxFlaggedRuns = n
	default:
		return fmt.Errorf("unknown threshold %q", key)
	}
	return nil
}

const telemetryQueryHelp = "Usage: goobers telemetry-query [--window <duration>] [--aggregate <name>]... [--threshold <k=v>]... [--format candidate-findings] [path]\n\n" +
	"Query the instance telemetry rollup for threshold-crossing failure and gate\n" +
	"patterns. The built-in connector stage writes a versioned candidate-findings\n" +
	"artifact to GOOBERS_INPUT_resultFile when declared, or to stdout otherwise.\n" +
	"With no --aggregate, all supported aggregates are evaluated. Threshold rates\n" +
	"are fractions from 0 through 1; count thresholds are positive integers.\n\n" +
	"Exit codes: 0 = OK (including a clean no-work result), 1 = business error,\n" +
	"2 = usage/IO error.\n"

// runTelemetryQuery implements the deterministic telemetry connector stage.
// It locates the instance through GOOBERS_INSTANCE_ROOT because the stage's
// working directory is its isolated worktree, not the instance root.
func runTelemetryQuery(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("telemetry-query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	window := fs.Duration("window", 24*time.Hour, "lookback window (for example 24h or 168h)")
	format := fs.String("format", telemetryQueryCandidateFormat, "artifact format (candidate-findings)")
	var aggregates telemetryAggregateValues
	fs.Var(&aggregates, "aggregate", "aggregate to detect; repeat for multiple (all, stage-failure-rate, error-signature, gate-noise)")
	thresholds := rollup.DefaultThresholds()
	fs.Var(&telemetryThresholdValue{thresholds: &thresholds}, "threshold",
		"threshold override k=v; repeat for multiple (min-samples, max-failure-rate, min-error-signature-count, min-gate-evaluations, max-gate-escalation-rate, max-flagged-runs)")
	fs.Usage = helpUsage(stderr, "telemetry-query")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *window <= 0 {
		pf(stderr, "error: --window must be a positive duration, got %s\n", *window)
		return 2
	}
	if *format != telemetryQueryCandidateFormat {
		pf(stderr, "error: --format must be %q, got %q\n", telemetryQueryCandidateFormat, *format)
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}

	since := time.Now().UTC().Add(-*window)
	l := layoutFor(providerStageRoot(pathArg))
	dbPath := l.TelemetryDB()
	if _, err := os.Stat(dbPath); err != nil {
		if !os.IsNotExist(err) {
			pf(stderr, "error: inspect telemetry rollup %s: %v\n", dbPath, err)
			return 1
		}
		pf(stderr, "note: no telemetry rollup at %s — if this persists, enable telemetry "+
			"(instance.yaml telemetry.enabled) and run at least one workflow under `goobers up`: %v\n", dbPath, err)
		result := newCandidateFindingsArtifact(*window, since, nil, telemetryQueryNoRollupNote)
		return writeCandidateFindingsArtifact(result, stdout, stderr)
	}
	db, err := openRollup(l, false)
	if err != nil {
		pf(stderr, "error: open telemetry rollup %s: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	result, err := detectCandidateFindings(db, *window, since, os.Getenv("GOOBERS_GAGGLE"), aggregates, thresholds)
	if err != nil {
		pf(stderr, "error: query candidate findings: %v\n", err)
		return 1
	}
	return writeCandidateFindingsArtifact(result, stdout, stderr)
}

func detectCandidateFindings(
	db *rollup.DB,
	window time.Duration,
	since time.Time,
	gaggle string,
	aggregates telemetryAggregateValues,
	thresholds rollup.Thresholds,
) (candidateFindingsArtifact, error) {
	findings, err := db.Detect(rollup.DetectRequest{
		StatsRequest: rollup.StatsRequest{Gaggle: gaggle, Since: since},
		Thresholds:   thresholds,
	})
	if err != nil {
		return candidateFindingsArtifact{}, err
	}

	filtered := make([]rollup.Finding, 0, len(findings))
	for _, finding := range findings {
		if !aggregates.includes(finding.Kind) {
			continue
		}
		if finding.FlaggedRuns == nil {
			finding.FlaggedRuns = []rollup.JournalPointer{}
		}
		filtered = append(filtered, finding)
	}
	note := ""
	if len(filtered) == 0 {
		note = telemetryQueryNoFindingsNote
	}
	return newCandidateFindingsArtifact(window, since, filtered, note), nil
}

func newCandidateFindingsArtifact(window time.Duration, since time.Time, findings []rollup.Finding, note string) candidateFindingsArtifact {
	if findings == nil {
		findings = []rollup.Finding{}
	}
	return candidateFindingsArtifact{
		Schema:   candidateFindingsSchemaVersion,
		Window:   window.String(),
		Since:    since,
		Findings: findings,
		NoWork:   len(findings) == 0,
		Note:     note,
	}
}

func writeCandidateFindingsArtifact(result candidateFindingsArtifact, stdout, stderr io.Writer) int {
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		pf(stderr, "error: encode candidate findings: %v\n", err)
		return 1
	}
	out = append(out, '\n')

	if rf := providerInput(executor.InputResultFile, ""); rf != "" {
		if err := os.WriteFile(rf, out, 0o644); err != nil {
			pf(stderr, "error: write result file %q: %v\n", rf, err)
			return 1
		}
		return 0
	}
	if _, err := stdout.Write(out); err != nil {
		return 2
	}
	return 0
}
