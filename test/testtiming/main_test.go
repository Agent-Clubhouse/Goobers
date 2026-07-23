package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTestEventsCollectsSortedTimingsAndStreamsOutput(t *testing.T) {
	t.Parallel()
	events := []testEvent{
		{Action: "pass", Package: "example/z", Elapsed: 1.5},
		{Action: "output", Package: "example/a", Test: "TestTwo", Output: "test output\n"},
		{Action: "pass", Package: "example/a", Test: "TestTwo", Elapsed: 0.2},
		{Action: "skip", Package: "example/a", Test: "TestOne", Elapsed: 0.1},
		{Action: "pass", Package: "example/a", Elapsed: 0.8},
	}
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}

	var output bytes.Buffer
	got, err := parseTestEvents(&input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != "test output\n" {
		t.Fatalf("streamed output = %q", output.String())
	}
	if len(got.Packages) != 2 || got.Packages[0].Package != "example/a" || got.Packages[1].Package != "example/z" {
		t.Fatalf("packages = %#v", got.Packages)
	}
	if len(got.Tests) != 2 || got.Tests[0].Test != "TestOne" || got.Tests[1].Test != "TestTwo" {
		t.Fatalf("tests = %#v", got.Tests)
	}
	if got.Tests[1].ElapsedSeconds != 0.2 || got.Tests[0].Status != "skip" {
		t.Fatalf("test timings = %#v", got.Tests)
	}
}

func TestParseTestEventsReturnsPartialArtifactOnInvalidJSON(t *testing.T) {
	t.Parallel()
	input := strings.NewReader("{\"Action\":\"pass\",\"Package\":\"example/a\",\"Elapsed\":1}\nnot-json\n")
	got, err := parseTestEvents(input, &bytes.Buffer{})
	if err == nil {
		t.Fatal("parseTestEvents() succeeded")
	}
	if len(got.Packages) != 1 || got.Packages[0].Package != "example/a" {
		t.Fatalf("partial artifact = %#v", got)
	}
}

func TestRunReportWritesComparisonAndSoftWarning(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	budgetPath := filepath.Join(directory, "budgets.json")
	currentPath := filepath.Join(directory, "current.json")
	previousPath := filepath.Join(directory, "previous.json")
	summaryPath := filepath.Join(directory, "summary.md")
	writeJSONFile(t, budgetPath, budgetFile{
		SchemaVersion: schemaVersion,
		Jobs: map[string]jobBudget{
			"unit": {
				BaselineSeconds: 100,
				BudgetSeconds:   120,
				Baseline:        "main at 2026-07-21",
			},
		},
	})
	writeJSONFile(t, currentPath, artifact{
		SchemaVersion:  schemaVersion,
		Job:            "unit",
		Platform:       "linux",
		Architecture:   "amd64",
		ElapsedSeconds: 130,
	})
	writeJSONFile(t, previousPath, artifact{
		SchemaVersion:  schemaVersion,
		Job:            "unit",
		ElapsedSeconds: 110,
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"report",
		"-budget", budgetPath,
		"-current", currentPath,
		"-previous", previousPath,
		"-summary", summaryPath,
	}, &stdout, &stderr, time.Now)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, &stderr)
	}
	if !strings.Contains(stdout.String(), "::warning title=Test timing budget exceeded::") {
		t.Fatalf("stdout missing soft warning:\n%s", &stdout)
	}
	if !strings.Contains(stdout.String(), "actual 130.0s / budget 120.0s") || !strings.Contains(stdout.String(), "change +20.0s (+18.2%)") {
		t.Fatalf("stdout missing comparison:\n%s", &stdout)
	}
	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), "| `unit` | 130.0s | 120.0s | -10.0s | 110.0s | +20.0s (+18.2%) | **OVER BUDGET** |") {
		t.Fatalf("summary =\n%s", summary)
	}
}

func TestRunReportStaysQuietWithinBudgetAndIgnoresMissingPrevious(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	budgetPath := filepath.Join(directory, "budgets.json")
	currentPath := filepath.Join(directory, "current.json")
	writeJSONFile(t, budgetPath, budgetFile{
		SchemaVersion: schemaVersion,
		Jobs: map[string]jobBudget{
			"unit": {BaselineSeconds: 100, BudgetSeconds: 150, Baseline: "measured main"},
		},
	})
	writeJSONFile(t, currentPath, artifact{
		SchemaVersion:  schemaVersion,
		Job:            "unit",
		ElapsedSeconds: 125,
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"report",
		"-budget", budgetPath,
		"-current", currentPath,
		"-previous", filepath.Join(directory, "missing.json"),
		"-summary", "",
	}, &stdout, &stderr, time.Now)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, &stderr)
	}
	if strings.Contains(stdout.String(), "::warning") {
		t.Fatalf("stdout contains warning:\n%s", &stdout)
	}
	if !strings.Contains(stdout.String(), "within budget") || !strings.Contains(stderr.String(), "previous timing unavailable") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestRunRejectsInvalidArgumentsAndBudgets(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr, time.Now); code != 2 {
		t.Fatalf("run(nil) = %d", code)
	}

	directory := t.TempDir()
	budgetPath := filepath.Join(directory, "budgets.json")
	currentPath := filepath.Join(directory, "current.json")
	writeJSONFile(t, budgetPath, budgetFile{
		SchemaVersion: schemaVersion,
		Jobs: map[string]jobBudget{
			"unit": {BaselineSeconds: 200, BudgetSeconds: 100, Baseline: "invalid"},
		},
	})
	writeJSONFile(t, currentPath, artifact{
		SchemaVersion:  schemaVersion,
		Job:            "unit",
		ElapsedSeconds: 90,
	})
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{
		"report",
		"-budget", budgetPath,
		"-current", currentPath,
		"-summary", "",
	}, &stdout, &stderr, time.Now); code != 1 {
		t.Fatalf("run(invalid budget) = %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid baselineSeconds") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestWriteArtifactCreatesParentDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "timing.json")
	want := artifact{SchemaVersion: schemaVersion, Job: "unit", ElapsedSeconds: 1}
	if err := writeArtifact(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Job != want.Job || got.ElapsedSeconds != want.ElapsedSeconds {
		t.Fatalf("artifact = %#v", got)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(value); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
