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

	"gopkg.in/yaml.v3"
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
	if failure.FailureSignature != "scheduler timed out after <duration>" {
		t.Fatalf("failure signature = %q", failure.FailureSignature)
	}
	if failure.FirstSeenRun != "12345" || failure.LastSeenRun != "12345" ||
		!failure.FirstSeenAt.Equal(first) || !failure.LastSeenAt.Equal(last) {
		t.Fatalf("failure sightings = %+v", failure)
	}
	if len(failure.Fingerprint) != 64 {
		t.Fatalf("fingerprint = %q, want a SHA-256 hex digest", failure.Fingerprint)
	}
}

func TestFailureFingerprintIncludesNormalizedSignature(t *testing.T) {
	t.Parallel()
	first := "runner_test.go:41: timed out after 1m2.5s at 0xc000012340"
	second := "runner_test.go:99: timed out after 3m4s at 0xDEADBEEF"
	different := "runner_test.go:41: got stopped, want ready"

	firstSignature := normalizeFailureSignature(first)
	secondSignature := normalizeFailureSignature(second)
	if firstSignature != secondSignature {
		t.Fatalf("volatile signatures differ: %q != %q", firstSignature, secondSignature)
	}
	firstFingerprint := failureFingerprint("./internal/runner", "TestResume", firstSignature)
	if got := failureFingerprint("./internal/runner", "TestResume", secondSignature); got != firstFingerprint {
		t.Fatalf("volatile fingerprints differ: %q != %q", got, firstFingerprint)
	}
	if got := failureFingerprint("./internal/runner", "TestResume", normalizeFailureSignature(different)); got == firstFingerprint {
		t.Fatalf("distinct assertion reused fingerprint %q", got)
	}
}

func TestNormalizeFailureSignatureBoundsLongAssertion(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("assertion detail ", failureSignatureLimit)
	first := normalizeFailureSignature(prefix + "first")
	second := normalizeFailureSignature(prefix + "second")
	if len([]rune(first)) != failureSignatureLimit {
		t.Fatalf("signature length = %d, want %d", len([]rune(first)), failureSignatureLimit)
	}
	if first == second {
		t.Fatal("long assertions with distinct suffixes produced the same signature")
	}
	if !strings.Contains(first, "… [sha256:") {
		t.Fatalf("bounded signature lacks digest suffix: %q", first)
	}
}

func TestNormalizePanicSignatureIncludesStableSite(t *testing.T) {
	t.Parallel()
	text := `panic: close of closed channel [recovered, repanicked]
goroutine 87 [running]:
testing.tRunner.func1.2({0x123, 0x456})
	/usr/local/go/src/testing/testing.go:1872 +0x123
github.com/goobers/goobers/internal/runner.resume()
	/tmp/work/internal/runner/resume.go:47 +0xabc`
	got := normalizeFailureSignature(text)
	want := "panic: close of closed channel [recovered, repanicked] | resume.go:47"
	if got != want {
		t.Fatalf("normalizeFailureSignature() = %q, want %q", got, want)
	}
}

func TestNormalizeRaceSignatureIncludesApplicationSite(t *testing.T) {
	t.Parallel()
	first := `==================
WARNING: DATA RACE
Write at 0x00c000012340 by goroutine 41:
  github.com/goobers/goobers/internal/runner.(*Runner).resume()
      /tmp/work-a/internal/runner/resume.go:47 +0xabc
==================`
	same := strings.ReplaceAll(strings.ReplaceAll(first, "0x00c000012340", "0xDEADBEEF"), "goroutine 41", "goroutine 99")
	different := strings.ReplaceAll(first, "resume.go:47", "resume.go:83")
	firstSignature := normalizeFailureSignature(first)
	if got := normalizeFailureSignature(same); got != firstSignature {
		t.Fatalf("volatile race signature = %q, want %q", got, firstSignature)
	}
	if got := normalizeFailureSignature(different); got == firstSignature {
		t.Fatalf("distinct race site reused signature %q", got)
	}
	if !strings.Contains(firstSignature, "resume.go:47") {
		t.Fatalf("race signature lacks application site: %q", firstSignature)
	}
}

func TestNormalizeRaceSignatureDistinguishesConflictingSite(t *testing.T) {
	t.Parallel()
	// Two races share the same first (new-access) site but conflict with a
	// different previous-access site — they must not collapse to the same
	// fingerprint (the original bug returned as soon as the first site's
	// frame was found, never scanning far enough to see the second site).
	template := `==================
WARNING: DATA RACE
Write at 0x00c000012340 by goroutine 41:
  github.com/goobers/goobers/internal/runner.(*Runner).resume()
      /tmp/work-a/internal/runner/resume.go:47 +0xabc

Previous %s at 0x00c000012340 by goroutine 12:
  github.com/goobers/goobers/internal/runner.(*Runner).%s()
      /tmp/work-a/internal/runner/%s +0xdef
==================`
	first := fmt.Sprintf(template, "write", "abort", "abort.go:99")
	second := fmt.Sprintf(template, "write", "release", "release.go:12")
	firstSignature := normalizeFailureSignature(first)
	secondSignature := normalizeFailureSignature(second)
	if firstSignature == secondSignature {
		t.Fatalf("distinct conflicting sites collapsed to one signature %q", firstSignature)
	}
	if !strings.Contains(firstSignature, "abort.go:99") {
		t.Fatalf("race signature lacks conflicting site: %q", firstSignature)
	}
	if !strings.Contains(secondSignature, "release.go:12") {
		t.Fatalf("race signature lacks conflicting site: %q", secondSignature)
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
		!strings.Contains(failures[0].FailureText, "timed out") ||
		failures[0].FailureSignature != "scheduler timed out" {
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
		`run ./test/ci full "$(MAKE)"`,
	)
	assertFileContains(t, filepath.Join(root, "test", "stress", "packages.txt"),
		"./internal/localscheduler",
		"./cmd/goobers",
		"./internal/harness",
		"./internal/httpapi",
	)
	assertFileContains(t, filepath.Join(root, ".github", "workflows", "stress.yml"),
		"schedule:",
		"workflow_dispatch:",
		"format('refs/pull/{0}/merge', inputs.pr)",
		"make stress",
		"actions/upload-artifact@v7",
		"flake-ledger:",
		"github.event_name == 'schedule'",
		"issues: write",
		"actions/download-artifact@v8",
		"go run ./test/flakeledger",
	)
}

func TestStressWorkflowTriggers(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", ".github", "workflows", "stress.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On map[string]yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, trigger := range []string{"pull_request", "pull_request_target"} {
		if _, ok := workflow.On[trigger]; ok {
			t.Fatalf("%s events must not trigger the Stress workflow", trigger)
		}
	}
	if _, ok := workflow.On["schedule"]; !ok {
		t.Fatal("Stress workflow has no scheduled trigger")
	}
	dispatch, ok := workflow.On["workflow_dispatch"]
	if !ok {
		t.Fatal("Stress workflow has no explicit dispatch trigger")
	}
	var config struct {
		Inputs map[string]struct {
			Required bool   `yaml:"required"`
			Type     string `yaml:"type"`
		} `yaml:"inputs"`
	}
	if err := dispatch.Decode(&config); err != nil {
		t.Fatalf("decode workflow_dispatch: %v", err)
	}
	pr, ok := config.Inputs["pr"]
	if !ok || !pr.Required || pr.Type != "string" {
		t.Fatalf("workflow_dispatch PR input = %+v, present = %v; want required string input", pr, ok)
	}
}

func TestGHCPEchoWorkflowTriggers(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", ".github", "workflows", "ghcp-echo.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]yaml.Node `yaml:"on"`
		Permissions map[string]string    `yaml:"permissions"`
		Jobs        map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, trigger := range []string{"pull_request", "pull_request_target"} {
		if _, ok := workflow.On[trigger]; ok {
			t.Fatalf("%s events must not trigger the GHCP Echo workflow", trigger)
		}
	}
	if _, ok := workflow.On["schedule"]; !ok {
		t.Fatal("GHCP Echo workflow has no scheduled trigger")
	}
	dispatch, ok := workflow.On["workflow_dispatch"]
	if !ok {
		t.Fatal("GHCP Echo workflow has no explicit dispatch trigger")
	}
	var config struct {
		Inputs map[string]struct {
			Required bool   `yaml:"required"`
			Type     string `yaml:"type"`
		} `yaml:"inputs"`
	}
	if err := dispatch.Decode(&config); err != nil {
		t.Fatalf("decode workflow_dispatch: %v", err)
	}
	pr, ok := config.Inputs["pr"]
	if !ok || !pr.Required || pr.Type != "string" {
		t.Fatalf("workflow_dispatch PR input = %+v, present = %v; want required string input", pr, ok)
	}
	if workflow.Permissions["pull-requests"] != "read" {
		t.Fatal("GHCP Echo workflow must grant pull-request read access")
	}

	jobNode, ok := workflow.Jobs["ghcp-echo"]
	if !ok {
		t.Fatal("GHCP Echo workflow has no ghcp-echo job")
	}
	var job struct {
		ContinueOnError bool   `yaml:"continue-on-error"`
		If              string `yaml:"if"`
		Steps           []struct {
			Name string            `yaml:"name"`
			Run  string            `yaml:"run"`
			Env  map[string]string `yaml:"env"`
			With map[string]string `yaml:"with"`
		} `yaml:"steps"`
	}
	if err := jobNode.Decode(&job); err != nil {
		t.Fatalf("decode ghcp-echo job: %v", err)
	}
	if !job.ContinueOnError {
		t.Fatal("GHCP Echo job must remain quarantined with continue-on-error")
	}
	if job.If != "vars.GHCP_ECHO_ENABLED == 'true'" {
		t.Fatal("GHCP Echo enablement guard changed")
	}
	if len(job.Steps) < 3 || job.Steps[0].Name != "Resolve trusted dispatch target" {
		t.Fatal("trusted PR resolution must precede secret access and checkout")
	}
	target := job.Steps[0].Run
	for _, value := range []string{".head.repo.full_name", `"$head_repository" != "$GITHUB_REPOSITORY"`, "refs/pull/${PR_NUMBER}/merge"} {
		if !strings.Contains(target, value) {
			t.Errorf("trusted PR resolution does not contain %q", value)
		}
	}
	if job.Steps[1].Env["MODEL_TOKEN"] != "${{ secrets.COPILOT_GITHUB_TOKEN }}" {
		t.Fatal("secret provisioning guard changed")
	}
	if job.Steps[2].With["ref"] != "${{ steps.target.outputs.ref || github.ref }}" {
		t.Fatal("checkout does not use the trusted PR target")
	}
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
