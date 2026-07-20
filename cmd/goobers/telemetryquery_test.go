package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestTelemetryQueryEmitsSchemaValidatedCandidateFindings(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)
	rebuildTelemetryQueryRollup(t, root)

	code, stdout, stderr := runArgs(t,
		"telemetry-query",
		"--window", "168h",
		"--aggregate", "stage-failure-rate",
		"--aggregate", "error-signature",
		"--threshold", "min-samples=1",
		"--threshold", "max-failure-rate=1",
		"--threshold", "min-error-signature-count=1",
		"--format", "candidate-findings",
		root,
	)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}

	validateCandidateFindings(t, []byte(stdout))
	var got candidateFindingsArtifact
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not parseable JSON: %v\n%s", err, stdout)
	}
	if got.Schema != candidateFindingsSchemaVersion {
		t.Fatalf("schema = %q, want %q", got.Schema, candidateFindingsSchemaVersion)
	}
	if got.NoWork || got.Note != "" {
		t.Fatalf("populated findings unexpectedly reported no-work: %+v", got)
	}

	kinds := map[rollup.FindingKind]rollup.Finding{}
	for _, finding := range got.Findings {
		kinds[finding.Kind] = finding
	}
	stage, ok := kinds[rollup.FindingStageFailureRate]
	if !ok {
		t.Fatalf("stage failure finding missing: %+v", got.Findings)
	}
	if stage.Threshold != 1 || stage.Metrics["failureRate"] != 1 {
		t.Fatalf("stage failure threshold boundary not honored: %+v", stage)
	}
	if len(stage.FlaggedRuns) != 1 || stage.FlaggedRuns[0].RunID != "fixture-run-1" {
		t.Fatalf("stage failure evidence = %+v, want fixture-run-1", stage.FlaggedRuns)
	}
	if _, ok := kinds[rollup.FindingErrorSignature]; !ok {
		t.Fatalf("error signature finding missing: %+v", got.Findings)
	}
	if !strings.Contains(stdout, `"flagged_runs"`) || strings.Contains(stdout, `"flaggedRuns"`) {
		t.Fatalf("artifact does not use the schema's flagged_runs field: %s", stdout)
	}
}

func TestTelemetryQueryAggregateFilter(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)
	rebuildTelemetryQueryRollup(t, root)

	code, stdout, stderr := runArgs(t,
		"telemetry-query",
		"--aggregate", "error-signature",
		"--threshold", "min-error-signature-count=1",
		root,
	)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got candidateFindingsArtifact
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != 1 || got.Findings[0].Kind != rollup.FindingErrorSignature {
		t.Fatalf("findings = %+v, want only error-signature", got.Findings)
	}
}

func TestTelemetryQueryScopesFindingsToRunGaggle(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	writeFixtureRunWithErrorForGaggle(t, l.ForGaggle("alpha"), "alpha-run", "alpha")
	writeFixtureRunWithErrorForGaggle(t, l.ForGaggle("bravo"), "bravo-run", "bravo")
	rebuildTelemetryQueryRollup(t, root)
	t.Setenv("GOOBERS_GAGGLE", "alpha")

	code, stdout, stderr := runArgs(t,
		"telemetry-query",
		"--aggregate", "error-signature",
		"--threshold", "min-error-signature-count=1",
		root,
	)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var got candidateFindingsArtifact
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %+v, want one error-signature finding", got.Findings)
	}
	flagged := got.Findings[0].FlaggedRuns
	if len(flagged) != 1 || flagged[0].RunID != "alpha-run" {
		t.Fatalf("flagged runs = %+v, want only alpha-run", flagged)
	}
	if strings.Contains(stdout, "bravo-run") {
		t.Fatalf("gaggle-scoped output leaked bravo-run: %s", stdout)
	}
}

func TestTelemetryQueryArtifactDeterministicForFixedInput(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)
	rebuildTelemetryQueryRollup(t, root)
	db, err := rollup.Open(instance.NewLayout(root).TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	thresholds := rollup.DefaultThresholds()
	thresholds.MinSamples = 1
	thresholds.MinErrorSignatureCount = 1
	since := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	aggregates := telemetryAggregateValues{telemetryAggregateStageFailureRate, telemetryAggregateErrorSignature}
	first, err := detectCandidateFindings(db, 24*time.Hour, since, "", aggregates, thresholds)
	if err != nil {
		t.Fatal(err)
	}
	second, err := detectCandidateFindings(db, 24*time.Hour, since, "", aggregates, thresholds)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("fixed query produced different artifacts:\n%s\n%s", firstJSON, secondJSON)
	}
	validateCandidateFindings(t, firstJSON)
}

func TestTelemetryQueryMissingRollupIsNoWork(t *testing.T) {
	root := initDemo(t)
	if err := os.Remove(instance.NewLayout(root).TelemetryDB()); err != nil {
		t.Fatal(err)
	}
	assertTelemetryQueryNoWork(t, root, "24h", telemetryQueryNoRollupNote)
}

func TestTelemetryQueryFreshRollupIsNoWork(t *testing.T) {
	assertTelemetryQueryNoWork(t, initDemo(t), "24h", telemetryQueryNoFindingsNote)
}

func TestTelemetryQueryEmptyWindowIsNoWork(t *testing.T) {
	root := initDemo(t)
	writeFixtureRunWithError(t, root)
	rebuildTelemetryQueryRollup(t, root)
	assertTelemetryQueryNoWork(t, root, "1ns", telemetryQueryNoFindingsNote)
}

func TestTelemetryQueryCorruptRollupIsError(t *testing.T) {
	root := initDemo(t)
	if err := os.WriteFile(instance.NewLayout(root).TelemetryDB(), []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runArgs(t, "telemetry-query", "--window", "24h", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "open telemetry rollup") {
		t.Fatalf("stderr = %q, want corrupt rollup error", stderr)
	}
}

func assertTelemetryQueryNoWork(t *testing.T, root, window, wantNote string) {
	t.Helper()
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), "candidate-findings.json")

	code, stdout, stderr := runArgs(t, "telemetry-query", "--window", window, root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want result written only to declared resultFile", stdout)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "candidate-findings.json"))
	if err != nil {
		t.Fatalf("read candidate-findings.json: %v", err)
	}
	validateCandidateFindings(t, data)
	var got candidateFindingsArtifact
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal candidate-findings.json: %v", err)
	}
	if !got.NoWork {
		t.Fatalf("noWork = false, want true: %s", data)
	}
	if got.Note != wantNote {
		t.Fatalf("note = %q, want %q", got.Note, wantNote)
	}
	if got.Findings == nil || len(got.Findings) != 0 {
		t.Fatalf("findings = %+v, want an empty array", got.Findings)
	}
}

func TestTelemetryQueryRejectsInvalidTypedFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"--bogus"}},
		{name: "nonpositive window", args: []string{"--window", "0s"}},
		{name: "unknown aggregate", args: []string{"--aggregate", "latency"}},
		{name: "malformed threshold", args: []string{"--threshold", "min-samples"}},
		{name: "unknown threshold", args: []string{"--threshold", "mystery=1"}},
		{name: "nonpositive count", args: []string{"--threshold", "min-samples=0"}},
		{name: "rate above one", args: []string{"--threshold", "max-failure-rate=1.1"}},
		{name: "unknown format", args: []string{"--format", "json"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := runArgs(t, append([]string{"telemetry-query"}, tc.args...)...)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (usage/IO error)", code)
			}
		})
	}
}

func rebuildTelemetryQueryRollup(t *testing.T, root string) {
	t.Helper()
	l := instance.NewLayout(root)
	runDirs, err := l.RunDirs()
	if err != nil {
		t.Fatalf("list run roots: %v", err)
	}
	if err := rollup.RebuildAll(l.TelemetryDB(), runDirs, l.SchedulerDir()); err != nil {
		t.Fatalf("rebuild rollup: %v", err)
	}
}

func validateCandidateFindings(t *testing.T, data []byte) {
	t.Helper()
	validator, err := validate.New()
	if err != nil {
		t.Fatalf("new schema validator: %v", err)
	}
	if err := validator.ValidateJSON(schemas.CandidateFindings, data); err != nil {
		t.Fatalf("candidate findings schema validation: %v\n%s", err, data)
	}
}
