package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// telemetryQuerySchema versions the connector's output. It is deliberately
// minimal: the typed, versioned candidate-findings contract is V1 scope
// (#148 / TEL-045, built around rollup.Detect). This is only the V0 floor that
// unblocks the work-nomination and tutor `gather-signals` stages (#232) — a
// bare signals digest (per-workflow / per-stage aggregates + top error
// signatures over the window) the nominator/analyst can reason over.
const telemetryQuerySchema = "goobers.dev/telemetry-query/v0"

type telemetryQueryResult struct {
	Schema          string                 `json:"schema"`
	Window          string                 `json:"window"`
	Since           time.Time              `json:"since"`
	Workflows       []workflowSignal       `json:"workflows"`
	Stages          []stageSignal          `json:"stages"`
	ErrorSignatures []errorSignatureSignal `json:"errorSignatures"`
	NoWork          bool                   `json:"noWork,omitempty"`
	Note            string                 `json:"note,omitempty"`
}

const telemetryQueryNoWorkNote = "no telemetry rollup yet"

type workflowSignal struct {
	Workflow      string  `json:"workflow"`
	TotalRuns     int     `json:"totalRuns"`
	CompletedRuns int     `json:"completedRuns"`
	FailedRuns    int     `json:"failedRuns"`
	SuccessRate   float64 `json:"successRate"`
	AvgDurationMs float64 `json:"avgDurationMs"`
}

type stageSignal struct {
	Stage             string  `json:"stage"`
	TotalAttempts     int     `json:"totalAttempts"`
	SucceededAttempts int     `json:"succeededAttempts"`
	FailedAttempts    int     `json:"failedAttempts"`
	SuccessRate       float64 `json:"successRate"`
}

type errorSignatureSignal struct {
	Code       string    `json:"code"`
	ErrorClass string    `json:"errorClass,omitempty"`
	Count      int       `json:"count"`
	LastSeen   time.Time `json:"lastSeen"`
}

// runTelemetryQuery implements `goobers telemetry-query` — the deterministic
// gather-signals stage for work-nomination and tutor (#232). It reads the
// instance's telemetry rollup and emits a small, stable signals JSON to a
// declared resultFile (GOOBERS_INPUT_resultFile) or stdout. Like the other
// stage subcommands (#131/#132) it locates the instance via
// GOOBERS_INSTANCE_ROOT (its cwd is the stage worktree, not the instance root).
func runTelemetryQuery(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("telemetry-query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	window := fs.Duration("window", 24*time.Hour, "lookback window for signals (e.g. 24h, 168h)")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers telemetry-query [--window <duration>] [path]\n\n"+
			"Query the instance's telemetry rollup for signals worth acting on over the\n"+
			"lookback window — per-workflow and per-stage success/failure aggregates and\n"+
			"the top recurring error signatures — and emit them as a small, stable JSON\n"+
			"document to a declared resultFile (GOOBERS_INPUT_resultFile) or stdout. This\n"+
			"is the V0 floor for the work-nomination/tutor gather-signals stage (#232);\n"+
			"the typed, versioned candidate-findings connector is V1 (#148).\n\n"+
			"Exit codes: 0 = OK (a missing or empty rollup is a clean no-work result),\n"+
			"1 = business error (unreadable/corrupt rollup, query error),\n"+
			"2 = usage/IO error.\n")
	}
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
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}

	l := layoutFor(providerStageRoot(pathArg))
	dbPath := l.TelemetryDB()
	if _, err := os.Stat(dbPath); err != nil {
		if !os.IsNotExist(err) {
			pf(stderr, "error: inspect telemetry rollup %s: %v\n", dbPath, err)
			return 1
		}
		since := time.Now().Add(-*window)
		result := telemetryQueryResult{Schema: telemetryQuerySchema, Window: window.String(), Since: since}
		result.NoWork = true
		result.Note = telemetryQueryNoWorkNote
		return writeTelemetryQueryResult(result, stdout, stderr)
	}
	db, err := rollup.Open(dbPath)
	if err != nil {
		pf(stderr, "error: open telemetry rollup %s: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	since := time.Now().Add(-*window)
	req := rollup.StatsRequest{Since: since}

	stats, err := db.Stats(req)
	if err != nil {
		pf(stderr, "error: query stats: %v\n", err)
		return 1
	}
	sigs, err := db.TopErrorSignatures(req, 20)
	if err != nil {
		pf(stderr, "error: query error signatures: %v\n", err)
		return 1
	}

	result := telemetryQueryResult{Schema: telemetryQuerySchema, Window: window.String(), Since: since}
	for _, r := range stats.Runs {
		result.Workflows = append(result.Workflows, workflowSignal{
			Workflow: r.Workflow, TotalRuns: r.TotalRuns, CompletedRuns: r.CompletedRuns,
			FailedRuns: r.FailedRuns, SuccessRate: r.SuccessRate, AvgDurationMs: r.AvgDurationMs,
		})
	}
	for _, s := range stats.Stages {
		result.Stages = append(result.Stages, stageSignal{
			Stage: s.Stage, TotalAttempts: s.TotalAttempts, SucceededAttempts: s.SucceededAttempts,
			FailedAttempts: s.FailedAttempts, SuccessRate: s.SuccessRate,
		})
	}
	for _, sig := range sigs {
		result.ErrorSignatures = append(result.ErrorSignatures, errorSignatureSignal{
			Code: sig.Code, ErrorClass: sig.ErrorClass, Count: sig.Count, LastSeen: sig.LastSeen,
		})
	}
	if len(result.Workflows) == 0 && len(result.Stages) == 0 && len(result.ErrorSignatures) == 0 {
		result.NoWork = true
		result.Note = telemetryQueryNoWorkNote
	}

	return writeTelemetryQueryResult(result, stdout, stderr)
}

func writeTelemetryQueryResult(result telemetryQueryResult, stdout, stderr io.Writer) int {
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		pf(stderr, "error: encode signals: %v\n", err)
		return 1
	}
	out = append(out, '\n')

	// A declared resultFile is written relative to the stage's cwd (its
	// worktree), exactly where the shell executor lifts it into an artifact for
	// the downstream nominate/analyze stage; otherwise emit to stdout (captured
	// as the stage's stdout.log artifact).
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
