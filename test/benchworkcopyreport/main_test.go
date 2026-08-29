package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWritesVersionedArtifactAndTrendSummary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "raw.json")
	historyDir := filepath.Join(dir, "history", "100")
	outputPath := filepath.Join(dir, "result.json")
	summaryPath := filepath.Join(dir, "summary.md")
	writeTestJSON(t, currentPath, benchmarkResult{
		Schema: "goobers.bench-workcopy/v2", ElapsedMs: 12_000,
		GOOS: "linux", GOARCH: "amd64", InitToFirstRunMs: 6_000,
		SecondRunMs: 2_000, Fixture: &fixtureResult{GenerateMs: 4_000},
	})
	writeTestJSON(t, filepath.Join(historyDir, "result.json"), artifact{
		SchemaVersion: schemaVersion, Job: jobName, RunID: "100", Revision: "old",
		ElapsedSeconds: 10, Phases: phaseTimings{
			FixtureGenerationSeconds: 3, InitToFirstRunSeconds: 5, SecondRunSeconds: 2,
		},
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-current", currentPath, "-history", filepath.Join(dir, "history"),
		"-out", outputPath, "-summary", summaryPath,
		"-run-id", "101", "-revision", "abc123", "-runner-class", "ubuntu-24.04",
		"-runner-name", "hosted", "-runner-image", "ubuntu24",
		"-cpu-model", "Example CPU", "-logical-cpus", "4", "-memory-bytes", "8589934592",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run = %d, stderr = %s", code, &stderr)
	}

	var got artifact
	readTestJSON(t, outputPath, &got)
	if got.SchemaVersion != 1 || got.Job != jobName || got.ElapsedSeconds != 12 {
		t.Fatalf("artifact identity/timing = %#v", got)
	}
	if got.Runner.Class != "ubuntu-24.04" || len(got.RecentRuns) != 1 {
		t.Fatalf("runner/history = %#v / %#v", got.Runner, got.RecentRuns)
	}
	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), "| Total | 12.0s | 10.0s | +2.0s (+20.0%) |") ||
		!strings.Contains(string(summary), "reporting-only") {
		t.Fatalf("summary =\n%s", summary)
	}
}

func TestRunRejectsMalformedCurrentArtifact(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	current := filepath.Join(dir, "raw.json")
	if err := os.WriteFile(current, []byte(`{"schema":"v2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-current", current, "-out", filepath.Join(dir, "out.json"),
		"-run-id", "1", "-revision", "abc", "-runner-class", "ubuntu-24.04",
		"-runner-name", "hosted", "-runner-image", "ubuntu24",
		"-cpu-model", "CPU", "-logical-cpus", "2", "-memory-bytes", "1024",
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "missing schema, elapsedMs, or platform metadata") {
		t.Fatalf("run = %d, stderr = %q", code, &stderr)
	}
}

func TestRunIgnoresMalformedAndIncompatibleHistory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "raw.json")
	historyDir := filepath.Join(dir, "history")
	outputPath := filepath.Join(dir, "result.json")
	writeTestJSON(t, currentPath, benchmarkResult{
		Schema: "goobers.bench-workcopy/v2", ElapsedMs: 12_000,
		GOOS: "linux", GOARCH: "amd64",
	})
	writeTestJSON(t, filepath.Join(historyDir, "100-valid.json"), artifact{
		SchemaVersion: schemaVersion, Job: jobName, RunID: "100",
		Revision: "old", ElapsedSeconds: 10,
	})
	writeTestJSON(t, filepath.Join(historyDir, "102-incompatible.json"), artifact{
		SchemaVersion: schemaVersion + 1, Job: jobName, RunID: "102",
		ElapsedSeconds: 11,
	})
	writeTestJSON(t, filepath.Join(historyDir, "103-incomplete.json"), artifact{
		SchemaVersion: schemaVersion, Job: jobName,
	})
	if err := os.WriteFile(filepath.Join(historyDir, "101-malformed.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-current", currentPath, "-history", historyDir, "-out", outputPath,
		"-run-id", "104", "-revision", "abc123", "-runner-class", "ubuntu-24.04",
		"-runner-name", "hosted", "-runner-image", "ubuntu24",
		"-cpu-model", "Example CPU", "-logical-cpus", "4", "-memory-bytes", "8589934592",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run = %d, stderr = %s", code, &stderr)
	}

	var got artifact
	readTestJSON(t, outputPath, &got)
	if len(got.RecentRuns) != 1 || got.RecentRuns[0].RunID != "100" {
		t.Fatalf("recent runs = %#v, want only valid history", got.RecentRuns)
	}
	for _, want := range []string{"warning:", "101-malformed.json", "102-incompatible.json", "103-incomplete.json"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", &stderr, want)
		}
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
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

func readTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, value); err != nil {
		t.Fatal(err)
	}
}
