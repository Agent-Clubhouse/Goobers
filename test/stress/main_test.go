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
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestLoadPackages(t *testing.T) {
	t.Parallel()
	packages, err := loadPackages(strings.NewReader(`
# timing-sensitive packages
./internal/localscheduler pass=45s
./internal/runner pass=1m # inline rationale
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []packageSpec{
		{Package: "./internal/localscheduler", Count: stressCount, Shards: 1, PassBudget: 45 * time.Second},
		{Package: "./internal/runner", Count: stressCount, Shards: 1, PassBudget: time.Minute},
	}
	if !slices.Equal(packages, want) {
		t.Fatalf("loadPackages() = %v, want %v", packages, want)
	}
}

func TestLoadPackagesAcceptsReviewedCountOverride(t *testing.T) {
	t.Parallel()
	packages, err := loadPackages(strings.NewReader(`
./internal/localscheduler pass=45s
./cmd/goobers count=2 pass=8m # full race suite exceeds the default stress budget
./internal/httpapi shards=4 pass=1m
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []packageSpec{
		{Package: "./internal/localscheduler", Count: stressCount, Shards: 1, PassBudget: 45 * time.Second},
		{Package: "./cmd/goobers", Count: 2, Shards: 1, PassBudget: 8 * time.Minute},
		{Package: "./internal/httpapi", Count: stressCount, Shards: 4, PassBudget: time.Minute},
	}
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
		{name: "absolute", list: "/internal/runner pass=1s\n", want: "relative"},
		{name: "invalid setting", list: "./internal/runner repeats=2 pass=1s\n", want: "count=N"},
		{name: "extra field", list: "./internal/runner count=2 extra\n", want: "key=value"},
		{name: "zero count", list: "./internal/runner count=0 pass=1s\n", want: "between 1"},
		{name: "negative count", list: "./internal/runner count=-1 pass=1s\n", want: "between 1"},
		{name: "excessive count", list: "./internal/runner count=21 pass=1s\n", want: "between 1"},
		{name: "duplicate", list: "./internal/runner pass=1s\n./internal/runner pass=1s\n", want: "duplicate"},
		{name: "repeated setting", list: "./internal/runner pass=1s pass=2s\n", want: "duplicate setting"},
		{name: "missing pass", list: "./internal/runner count=2\n", want: "missing pass"},
		{name: "zero pass", list: "./internal/runner pass=0s\n", want: "positive duration"},
		{name: "zero shards", list: "./internal/runner shards=0 pass=1s\n", want: "shards must be"},
		{name: "excessive shards", list: "./internal/runner shards=65 pass=1s\n", want: "shards must be"},
		{name: "over budget", list: "./cmd/goobers pass=20m\n", want: "exceeds the 30m0s per-shard timeout"},
		{name: "over budget shards", list: "./cmd/goobers pass=20m shards=8\n", want: "raise shards="},
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
	for _, count := range []int{stressCount, 2} {
		got := goTestArgs("./internal/localscheduler", count, 42, "")
		want := []string{
			"test", "-json", "-race", "-count=" + strconv.Itoa(count), "-timeout=30m0s", "-shuffle=42", "./internal/localscheduler",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("goTestArgs(count=%d) = %v, want %v", count, got, want)
		}
	}
	sharded := goTestArgs("./cmd/goobers", 20, 42, "^(?:TestOne|TestTwo)$")
	wantSharded := []string{
		"test", "-json", "-race", "-count=20", "-timeout=30m0s", "-shuffle=42",
		"-run=^(?:TestOne|TestTwo)$", "./cmd/goobers",
	}
	if !slices.Equal(sharded, wantSharded) {
		t.Fatalf("goTestArgs(shard) = %v, want %v", sharded, wantSharded)
	}
	if got := goListArgs("./cmd/goobers"); !slices.Equal(got, []string{"test", "-race", "-list=^Test", "./cmd/goobers"}) {
		t.Fatalf("goListArgs() = %v", got)
	}
	if stressTimeout <= 10*time.Minute {
		t.Fatalf("stressTimeout = %v, want more than Go's 10m default (#3167)", stressTimeout)
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
			if err := os.WriteFile(packageList, []byte("./internal/localscheduler pass=45s\n"), 0o644); err != nil {
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

	result, failures, err := runner.runShard(context.Background(), shardSpec{
		ID:      "internal_localscheduler",
		Package: "./internal/localscheduler",
		Count:   stressCount,
		Shards:  1,
	})
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
		[]shardSpec{{ID: "internal_localscheduler", Package: "./internal/localscheduler", Count: stressCount, Shards: 1}},
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

func TestExecuteStressReportsPackageSpecificCounts(t *testing.T) {
	t.Setenv("GOOBERS_STRESS_HELPER", "pass")
	runner := processRunner{
		command:   helperCommand,
		goCommand: "go",
		outputDir: t.TempDir(),
		runID:     "run-counts",
		count:     stressCount,
		seed:      43,
		stdout:    &bytes.Buffer{},
		stderr:    &bytes.Buffer{},
		now:       time.Now,
	}
	summary, _, err := executeStress(context.Background(), runner, []shardSpec{
		{ID: "internal_localscheduler", Package: "./internal/localscheduler", Count: stressCount, Shards: 1},
		{ID: "cmd_goobers", Package: "./cmd/goobers", Count: 2, Shards: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Count != stressCount || len(summary.Packages) != 2 ||
		summary.Packages[0].Count != stressCount || summary.Packages[1].Count != 2 {
		t.Fatalf("summary package counts = %+v, want default and override", summary)
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
		"./cmd/goobers pass=",
		"./internal/harness",
		"./internal/httpapi",
	)
	assertFileContains(t, filepath.Join(root, ".github", "workflows", "stress.yml"),
		"schedule:",
		"workflow_dispatch:",
		"format('refs/pull/{0}/merge', inputs.pr)",
		"go run ./test/stress -list",
		"fromJSON(needs.plan.outputs.shards)",
		"make stress STRESS_SHARD=",
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
	if slices.Contains(os.Args, "-list=^Test") {
		for _, name := range []string{"TestAlpha", "TestBeta", "TestGamma", "TestDelta"} {
			fmt.Println(name)
		}
		fmt.Printf("ok  \t%s\t0.001s\n", pkg)
		os.Exit(0)
	}
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

func TestExpandShardsAssignsStableIdentifiers(t *testing.T) {
	t.Parallel()
	shards := expandShards([]packageSpec{
		{Package: "./internal/localscheduler", Count: stressCount, Shards: 1, PassBudget: 45 * time.Second},
		{Package: "./cmd/goobers", Count: 4, Shards: 3, PassBudget: time.Minute},
	})
	want := []shardSpec{
		{ID: "internal_localscheduler", Package: "./internal/localscheduler", Count: stressCount, Index: 0, Shards: 1},
		{ID: "cmd_goobers-01of03", Package: "./cmd/goobers", Count: 4, Index: 0, Shards: 3},
		{ID: "cmd_goobers-02of03", Package: "./cmd/goobers", Count: 4, Index: 1, Shards: 3},
		{ID: "cmd_goobers-03of03", Package: "./cmd/goobers", Count: 4, Index: 2, Shards: 3},
	}
	if !slices.Equal(shards, want) {
		t.Fatalf("expandShards() = %+v, want %+v", shards, want)
	}
	if !slices.Equal(shardIDs(shards[1:]), []string{"cmd_goobers-01of03", "cmd_goobers-02of03", "cmd_goobers-03of03"}) {
		t.Fatalf("shardIDs() = %v", shardIDs(shards[1:]))
	}
	if _, err := selectShard(shards, "cmd_goobers"); err == nil ||
		!strings.Contains(err.Error(), "unknown shard") {
		t.Fatalf("selectShard(unknown) error = %v", err)
	}
	selected, err := selectShard(shards, "cmd_goobers-02of03")
	if err != nil || selected.Index != 1 {
		t.Fatalf("selectShard() = %+v, %v", selected, err)
	}
}

func TestShardRunPatternPartitionsListedTests(t *testing.T) {
	t.Setenv("GOOBERS_STRESS_HELPER", "pass")
	runner := processRunner{
		command:   helperCommand,
		goCommand: "go",
		outputDir: t.TempDir(),
		stdout:    &bytes.Buffer{},
		stderr:    &bytes.Buffer{},
		now:       time.Now,
	}
	// The helper lists TestAlpha, TestBeta, TestGamma, TestDelta.
	patterns := make([]string, 0, 2)
	for index := range 2 {
		pattern, err := runner.shardRunPattern(context.Background(), shardSpec{
			ID:      shardID("./cmd/goobers", index, 2),
			Package: "./cmd/goobers",
			Index:   index,
			Shards:  2,
		})
		if err != nil {
			t.Fatal(err)
		}
		patterns = append(patterns, pattern)
	}
	want := []string{"^(?:TestAlpha|TestGamma)$", "^(?:TestBeta|TestDelta)$"}
	if !slices.Equal(patterns, want) {
		t.Fatalf("shard run patterns = %v, want %v", patterns, want)
	}

	unsharded, err := runner.shardRunPattern(context.Background(), shardSpec{Package: "./cmd/goobers", Shards: 1})
	if err != nil || unsharded != "" {
		t.Fatalf("shardRunPattern(single shard) = %q, %v", unsharded, err)
	}

	if _, err := runner.shardRunPattern(context.Background(), shardSpec{
		ID:      "cmd_goobers-09of09",
		Package: "./cmd/goobers",
		Index:   8,
		Shards:  9,
	}); err == nil || !strings.Contains(err.Error(), "selected no tests") {
		t.Fatalf("shardRunPattern(empty shard) error = %v", err)
	}
}

// TestEnrolledBudgetsFitTheShardTimeout is the guard #4222 asks for: the
// checked-in enrollment may not imply a per-shard budget the nightly job
// cannot pay, and the job that pays it must be allowed to run that long.
func TestEnrolledBudgetsFitTheShardTimeout(t *testing.T) {
	t.Parallel()
	file, err := os.Open(filepath.Join("packages.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	packages, err := loadPackages(file)
	if err != nil {
		t.Fatalf("checked-in enrollment does not load: %v", err)
	}
	for _, spec := range packages {
		if spec.PassBudget <= 0 {
			t.Errorf("%s declares no reviewed pass cost", spec.Package)
		}
		if budget := spec.shardBudget(); budget > stressTimeout {
			t.Errorf("%s shard budget %v exceeds the %v per-shard timeout", spec.Package, budget, stressTimeout)
		}
	}
	if got, want := enrolledCountFor(t, packages, "./cmd/goobers"), 2; got <= want {
		t.Errorf("./cmd/goobers runs %d repetition(s) per shard, want more than %d (#4222)", got, want)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "stress.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			TimeoutMinutes int `yaml:"timeout-minutes"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	if budget := workflow.Jobs["stress"].TimeoutMinutes; time.Duration(budget)*time.Minute < stressTimeout {
		t.Fatalf("stress job timeout-minutes = %d, want at least the %v per-shard timeout", budget, stressTimeout)
	}
}

func enrolledCountFor(t *testing.T, packages []packageSpec, pkg string) int {
	t.Helper()
	for _, spec := range packages {
		if spec.Package == pkg {
			return spec.Count
		}
	}
	t.Fatalf("%s is not enrolled", pkg)
	return 0
}
