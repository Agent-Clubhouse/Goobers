package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadPackages(t *testing.T) {
	t.Parallel()
	packages, err := loadPackages(strings.NewReader(`
# timing-sensitive packages
./internal/localscheduler
./internal/runner # inline rationale
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"./internal/localscheduler", "./internal/runner"}
	if !slices.Equal(packages, want) {
		t.Fatalf("loadPackages() = %v, want %v", packages, want)
	}
}

func TestLoadPackagesRejectsInvalidEnrollment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		list string
		want string
	}{
		{name: "empty", list: "# comments only\n", want: "empty"},
		{name: "absolute", list: "/internal/runner\n", want: "relative"},
		{name: "whitespace", list: "./internal/runner ./internal/engine\n", want: "relative"},
		{name: "duplicate", list: "./internal/runner\n./internal/runner\n", want: "duplicate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadPackages(strings.NewReader(test.list))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadPackages() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestFailureCollectorAggregatesFingerprintOccurrences(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	last := first.Add(time.Second)
	collector := newFailureCollector("./internal/localscheduler", "12345")
	for index, observed := range []time.Time{first, last} {
		collector.consume(testEvent{Action: "run", Test: "TestTick"})
		collector.consume(testEvent{
			Action: "output",
			Test:   "TestTick",
			Output: fmt.Sprintf("scheduler timed out after %dms\n", index+10),
		})
		collector.consume(testEvent{Action: "fail", Test: "TestTick", Time: observed})
	}
	if len(collector.failures) != 1 {
		t.Fatalf("failures = %d, want one fingerprint: %+v", len(collector.failures), collector.failures)
	}
	failure := collector.failures[0]
	if failure.Package != "./internal/localscheduler" || failure.Test != "TestTick" ||
		failure.FailureText != "scheduler timed out after 10ms" || failure.Occurrences != 2 {
		t.Fatalf("failure = %+v", failure)
	}
	if failure.FirstSeenRun != "12345" || failure.LastSeenRun != "12345" ||
		!failure.FirstSeenAt.Equal(first) || !failure.LastSeenAt.Equal(last) {
		t.Fatalf("failure sightings = %+v", failure)
	}
	if len(failure.Fingerprint) != 64 {
		t.Fatalf("fingerprint = %q, want a SHA-256 hex digest", failure.Fingerprint)
	}
}

func TestGoTestArgsLockStressFlags(t *testing.T) {
	t.Parallel()
	got := goTestArgs("./internal/localscheduler", stressCount, 42)
	want := []string{
		"test", "-json", "-race", "-count=20", "-shuffle=42", "./internal/localscheduler",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("goTestArgs() = %v, want %v", got, want)
	}
}

func TestRunWritesPassAndPackageFailureReports(t *testing.T) {
	for _, test := range []struct {
		name       string
		mode       string
		wantCode   int
		wantStatus string
		wantTest   string
	}{
		{name: "pass", mode: "pass", wantCode: 0, wantStatus: "pass"},
		{name: "package failure", mode: "package-fail", wantCode: 1, wantStatus: "fail", wantTest: "(package)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GOOBERS_STRESS_HELPER", test.mode)
			dir := t.TempDir()
			packageList := filepath.Join(dir, "packages.txt")
			if err := os.WriteFile(packageList, []byte("./internal/localscheduler\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			outputDir := filepath.Join(dir, "results")
			var stdout, stderr bytes.Buffer
			now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
			clock := func() time.Time {
				now = now.Add(time.Second)
				return now
			}
			code := run(
				[]string{"-packages", packageList, "-output", outputDir, "-seed", "42"},
				&stdout,
				&stderr,
				os.Getenv,
				clock,
				helperCommand,
			)
			if code != test.wantCode {
				t.Fatalf("run() = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, test.wantCode, &stdout, &stderr)
			}

			var summary summaryReport
			readJSON(t, filepath.Join(outputDir, "summary.json"), &summary)
			if summary.Status != test.wantStatus || summary.Count != stressCount || summary.Seed != 42 {
				t.Fatalf("summary = %+v", summary)
			}
			var failures failuresReport
			readJSON(t, filepath.Join(outputDir, "failures.json"), &failures)
			if test.wantTest == "" {
				if len(failures.Failures) != 0 {
					t.Fatalf("failures = %+v, want none", failures.Failures)
				}
			} else if len(failures.Failures) != 1 || failures.Failures[0].Test != test.wantTest ||
				!strings.Contains(failures.Failures[0].FailureText, "package setup failed") {
				t.Fatalf("failures = %+v", failures.Failures)
			}
		})
	}
}

func TestParseOptionsRejectsInvalidArguments(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"positional"},
		{"-seed", "-1"},
		{"-output", ""},
	} {
		var stderr bytes.Buffer
		if _, err := parseOptions(args, &stderr, func(string) string { return "" }); err == nil {
			t.Fatalf("parseOptions(%v) succeeded", args)
		}
	}
}

func TestProcessRunnerWritesStructuredFailureArtifacts(t *testing.T) {
	t.Setenv("GOOBERS_STRESS_HELPER", "fail")
	outputDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	runner := processRunner{
		command:   helperCommand,
		goCommand: "go",
		outputDir: outputDir,
		runID:     "run-7",
		count:     stressCount,
		seed:      42,
		stdout:    &stdout,
		stderr:    &stderr,
		now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
	}

	result, failures, err := runner.runPackage(context.Background(), "./internal/localscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "fail" || result.StructuredFailures != 1 || result.TestElapsedSecs != 0.25 {
		t.Fatalf("result = %+v", result)
	}
	if len(failures) != 1 || failures[0].Test != "TestTick" ||
		!strings.Contains(failures[0].FailureText, "timed out") {
		t.Fatalf("failures = %+v", failures)
	}
	raw, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(result.EventLog)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"Test":"TestTick"`)) {
		t.Fatalf("raw event log missing test event:\n%s", raw)
	}
	stderrRaw, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(result.StderrLog)))
	if err != nil {
		t.Fatal(err)
	}
	if string(stderrRaw) != "helper diagnostic\n" || stderr.String() != "helper diagnostic\n" {
		t.Fatalf("stderr artifact/output = %q / %q", stderrRaw, stderr.String())
	}
}

func TestExecuteStressAndWriteReports(t *testing.T) {
	t.Setenv("GOOBERS_STRESS_HELPER", "pass")
	outputDir := t.TempDir()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	runner := processRunner{
		command:   helperCommand,
		goCommand: "go",
		outputDir: outputDir,
		runID:     "run-8",
		count:     stressCount,
		seed:      43,
		stdout:    &bytes.Buffer{},
		stderr:    &bytes.Buffer{},
		now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
	}
	summary, failures, err := executeStress(
		context.Background(),
		runner,
		[]string{"./internal/localscheduler"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != "pass" || len(summary.Packages) != 1 || len(failures.Failures) != 0 {
		t.Fatalf("reports = %+v / %+v", summary, failures)
	}
	summary.Run = runMetadata{RunID: "run-8"}
	failures.Run = summary.Run
	if err := writeReports(outputDir, summary, failures); err != nil {
		t.Fatal(err)
	}
	var decoded summaryReport
	raw, err := os.ReadFile(filepath.Join(outputDir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != reportSchema || decoded.Count != 20 ||
		decoded.Seed != 43 || decoded.Run.RunID != "run-8" {
		t.Fatalf("decoded summary = %+v", decoded)
	}
}

func TestSyntheticFailureTruncatesText(t *testing.T) {
	t.Parallel()
	failure := syntheticFailure(
		"./internal/localscheduler",
		"run-9",
		strings.Repeat("x", failureTextLimit+1),
		time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	)
	if !failure.FailureTextTruncated || len(failure.FailureText) != failureTextLimit ||
		failure.Test != "(package)" {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestWriteReportsRejectsInvalidOutput(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeReports(path, summaryReport{}, failuresReport{}); err == nil {
		t.Fatal("writeReports() succeeded with a file as the output directory")
	}

	badJSON := filepath.Join(t.TempDir(), "bad.json")
	if err := writeJSON(badJSON, make(chan int)); err == nil {
		t.Fatal("writeJSON() accepted an unsupported value")
	}
}

func TestMetadataFromEnvironment(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"GITHUB_RUN_ID":      "123",
		"GITHUB_RUN_ATTEMPT": "2",
		"GITHUB_EVENT_NAME":  "schedule",
		"GITHUB_REPOSITORY":  "Agent-Clubhouse/Goobers",
		"GITHUB_SERVER_URL":  "https://github.com",
		"GITHUB_SHA":         "abc123",
	}
	getenv := func(name string) string { return values[name] }
	started := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	metadata := metadataFromEnvironment(getenv, started)
	if metadata.RunID != "123" || metadata.RunAttempt != "2" || metadata.Trigger != "schedule" ||
		metadata.URL != "https://github.com/Agent-Clubhouse/Goobers/actions/runs/123" ||
		metadata.SHA != "abc123" || !metadata.StartedAt.Equal(started) {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestRepositoryStressWiring(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	assertFileContains(t, filepath.Join(root, "Makefile"),
		"run ./test/stress",
		"verify-full: ci test-integration-strict test-e2e test-envtest cover-check sandbox-check linux-node-validation test-shipped-workflows stress",
	)
	assertFileContains(t, filepath.Join(root, "test", "stress", "packages.txt"),
		"./internal/localscheduler",
	)
	assertFileContains(t, filepath.Join(root, ".github", "workflows", "stress.yml"),
		"schedule:",
		"workflow_dispatch:",
		"types: [labeled]",
		"github.event.label.name == '/stress'",
		"make stress",
		"actions/upload-artifact@v5",
	)
}

func assertFileContains(t *testing.T, path string, values ...string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if !bytes.Contains(raw, []byte(value)) {
			t.Errorf("%s does not contain %q", path, value)
		}
	}
}

func readJSON(t *testing.T, path string, destination any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		t.Fatal(err)
	}
}

func helperCommand(ctx context.Context, _ string, args ...string) *exec.Cmd {
	helperArgs := []string{"-test.run=^TestStressHelperProcess$", "--"}
	return exec.CommandContext(ctx, os.Args[0], append(helperArgs, args...)...)
}

func TestStressHelperProcess(t *testing.T) {
	mode := os.Getenv("GOOBERS_STRESS_HELPER")
	if mode == "" {
		return
	}
	pkg := os.Args[len(os.Args)-1]
	events := []testEvent{{Time: time.Now(), Action: "start", Package: pkg}}
	switch mode {
	case "fail":
		events = append(events,
			testEvent{Time: time.Now(), Action: "run", Package: pkg, Test: "TestTick"},
			testEvent{Time: time.Now(), Action: "output", Package: pkg, Test: "TestTick", Output: "scheduler timed out\n"},
			testEvent{Time: time.Now(), Action: "fail", Package: pkg, Test: "TestTick", Elapsed: 0.1},
			testEvent{Time: time.Now(), Action: "fail", Package: pkg, Elapsed: 0.25},
		)
	case "package-fail":
		events = append(events,
			testEvent{Time: time.Now(), Action: "output", Package: pkg, Output: "package setup failed\n"},
			testEvent{Time: time.Now(), Action: "fail", Package: pkg, Elapsed: 0.25},
		)
	default:
		events = append(events,
			testEvent{Time: time.Now(), Action: "run", Package: pkg, Test: "TestTick"},
			testEvent{Time: time.Now(), Action: "pass", Package: pkg, Test: "TestTick", Elapsed: 0.1},
			testEvent{Time: time.Now(), Action: "pass", Package: pkg, Elapsed: 0.2},
		)
	}
	encoder := json.NewEncoder(os.Stdout)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if mode != "pass" {
		fmt.Fprintln(os.Stderr, "helper diagnostic")
		os.Exit(1)
	}
	os.Exit(0)
}
