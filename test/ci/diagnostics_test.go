package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const goTestFailureOutput = `==> test
ok  	github.com/goobers/goobers/internal/fine	0.5s
--- FAIL: TestOuter (0.02s)
    --- FAIL: TestOuter/sub_case (0.01s)
        outer_test.go:12: want 1, got 2
    --- PASS: TestOuter/other (0.00s)
--- FAIL: TestSecond (0.00s)
    second_test.go:7: boom
FAIL
FAIL	github.com/goobers/goobers/internal/broken	0.031s
# github.com/goobers/goobers/internal/nobuild
internal/nobuild/x.go:3:1: syntax error
FAIL	github.com/goobers/goobers/internal/nobuild [build failed]
FAIL
<== test (elapsed 3s)
ci: test: exit status 1`

func TestExtractFailuresAttributesTestsToTheirPackage(t *testing.T) {
	t.Parallel()
	report := extractFailures(strings.Split(goTestFailureOutput, "\n"))
	wantTests := []failedTest{
		{Package: "github.com/goobers/goobers/internal/broken", Name: "TestOuter"},
		{Package: "github.com/goobers/goobers/internal/broken", Name: "TestOuter/sub_case"},
		{Package: "github.com/goobers/goobers/internal/broken", Name: "TestSecond"},
	}
	if !slices.Equal(report.Tests, wantTests) {
		t.Fatalf("tests = %+v, want %+v", report.Tests, wantTests)
	}
	wantPackages := []failedPackage{
		{Name: "github.com/goobers/goobers/internal/broken", Reason: "tests failed"},
		{Name: "github.com/goobers/goobers/internal/nobuild", Reason: "build failed"},
	}
	if !slices.Equal(report.Packages, wantPackages) {
		t.Fatalf("packages = %+v, want %+v", report.Packages, wantPackages)
	}
	excerpt := strings.Join(report.Excerpt, "\n")
	for _, want := range []string{"outer_test.go:12: want 1, got 2", "second_test.go:7: boom"} {
		if !strings.Contains(excerpt, want) {
			t.Errorf("excerpt is missing %q:\n%s", want, excerpt)
		}
	}
	if strings.Contains(excerpt, "internal/fine") || strings.Contains(excerpt, "syntax error") {
		t.Errorf("excerpt holds output of other packages:\n%s", excerpt)
	}
}

// TestExtractFailuresKeepsVerboseOutputWithItsTest covers the -v / -json
// relay layout the unit jobs print, where a test's log lines come before its
// `--- FAIL:` line and parallel tests interleave between `=== CONT` markers.
func TestExtractFailuresKeepsVerboseOutputWithItsTest(t *testing.T) {
	t.Parallel()
	lines := strings.Split(strings.Join([]string{
		"=== RUN   TestOuter",
		"=== PAUSE TestOuter",
		"=== RUN   TestPeer",
		"=== PAUSE TestPeer",
		"=== CONT  TestOuter",
		"=== RUN   TestOuter/sub_case",
		"    outer_test.go:12: want 1, got 2",
		"=== CONT  TestPeer",
		"    peer_test.go:3: peer chatter",
		"=== NAME  TestOuter/sub_case",
		"    outer_test.go:13: still wrong",
		"--- FAIL: TestOuter (0.02s)",
		"    --- FAIL: TestOuter/sub_case (0.01s)",
		"--- PASS: TestPeer (0.00s)",
		"=== RUN   TestSecond",
		"    second_test.go:7: boom",
		"--- FAIL: TestSecond (0.00s)",
		"FAIL",
		"FAIL\tgithub.com/goobers/goobers/internal/broken\t0.03s",
	}, "\n"), "\n")
	report := extractFailures(lines)
	if len(report.Tests) != 3 {
		t.Fatalf("tests = %+v, want TestOuter, its subtest, and TestSecond", report.Tests)
	}
	blocks := strings.Split(strings.Join(report.Excerpt, "\n"), "\n\n")
	block := func(header string) string {
		for _, b := range blocks {
			if strings.Contains(b, header) {
				return b
			}
		}
		t.Fatalf("no excerpt block for %q in:\n%s", header, strings.Join(report.Excerpt, "\n"))
		return ""
	}
	outer := block("--- FAIL: TestOuter (")
	second := block("--- FAIL: TestSecond (")
	for _, want := range []string{"outer_test.go:12: want 1, got 2", "outer_test.go:13: still wrong"} {
		if !strings.Contains(outer, want) {
			t.Errorf("TestOuter block is missing %q:\n%s", want, outer)
		}
	}
	for _, foreign := range []string{"peer_test.go", "second_test.go", "TestSecond"} {
		if strings.Contains(outer, foreign) {
			t.Errorf("TestOuter block holds %q from another test:\n%s", foreign, outer)
		}
	}
	if !strings.Contains(second, "second_test.go:7: boom") || strings.Contains(second, "outer_test.go") {
		t.Errorf("TestSecond block = \n%s", second)
	}
}

func TestClassifyJobSeparatesTestCheckAndInfrastructureFailures(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := started.Add(50 * time.Minute)
	failed := func(check string, lines ...string) runDiagnosis {
		return runDiagnosis{
			Run:    driverRun{Result: runResultFailed, FailedCheck: check},
			Report: extractFailures(lines),
		}
	}
	interrupted := runDiagnosis{Run: driverRun{Result: runResultRunning, Checks: []checkTiming{
		{Label: "lint", Started: started, Finished: true},
		{Label: "test", Started: started.Add(5 * time.Minute)},
	}}}
	passed := runDiagnosis{Run: driverRun{Result: runResultPassed}}
	cases := []struct {
		name, status, class, category, detail string
		runs                                  []runDiagnosis
	}{
		{"job passed", "success", "passed", "none", "", []runDiagnosis{passed}},
		{"failing test", "failure", "test-failure", categoryTest, "check test: 1 failing test(s) in 1 package(s)",
			[]runDiagnosis{failed("test", "--- FAIL: TestX (0.00s)", "FAIL\texample.com/p\t0.1s")}},
		{"hung test", "failure", "test-timeout", categoryTest, "-timeout of 30m0s",
			[]runDiagnosis{failed("test", "panic: test timed out after 30m0s", "FAIL\texample.com/p\t1800.1s")}},
		{"package panic without a failing test", "failure", "test-failure", categoryTest, "without a failing test",
			[]runDiagnosis{failed("test", "panic: boom", "FAIL\texample.com/p\t0.1s")}},
		{"compile error", "failure", "build-failure", categoryCheck, "did not compile",
			[]runDiagnosis{failed("test", "FAIL\texample.com/p [build failed]")}},
		{"lint finding", "failure", "check-failure", categoryCheck, "check lint failed",
			[]runDiagnosis{failed("lint", "x.go:1:1: unused (unused)")}},
		{"step timeout", "failure", "step-timeout", categoryInfra, "check test had run for 45m0s",
			[]runDiagnosis{passed, interrupted}},
		{"cancelled mid-check", "cancelled", "cancelled", categoryInfra, "check test had run for 45m0s",
			[]runDiagnosis{interrupted}},
		{"cancelled outside the driver", "cancelled", "cancelled", categoryInfra, "no CI driver check was running", nil},
		{"setup failure", "failure", "outside-driver-failure", categoryOutsideDriver, "never ran", nil},
		{"later step failed", "failure", "post-driver-failure", categoryOutsideDriver, "every CI driver check passed",
			[]runDiagnosis{passed}},
	}
	for _, tc := range cases {
		got := classifyJob(tc.status, tc.runs, now)
		if got.Class != tc.class || got.Category != tc.category || !strings.Contains(got.Detail, tc.detail) {
			t.Errorf("%s: classifyJob = %+v, want class %s category %s detail containing %q",
				tc.name, got, tc.class, tc.category, tc.detail)
		}
	}
}

// TestDiagnoseReportsRecordedDriverRuns drives the recorder the way the CI
// driver does (a failed invocation, then one killed mid-check) and checks
// what the diagnose step publishes.
func TestDiagnoseReportsRecordedDriverRuns(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { clock = clock.Add(time.Second); return clock }

	failing, err := startDiagnostics(root, "group unit", now)
	if err != nil {
		t.Fatal(err)
	}
	var console bytes.Buffer
	stdout, stderr := failing.writers(&console, &console)
	exec := &fakeExecutor{failCommands: map[string]bool{"test": true}}
	runErr := executeChecks(recordingExecutor{inner: writingExecutor{exec, stdout}, recorder: failing},
		[]check{{label: "schema"}, {label: "test"}}, stdout, stderr)
	failing.finish(runErr)
	if !strings.Contains(console.String(), "--- FAIL: TestOuter") {
		t.Fatalf("console lost the check output:\n%s", console.String())
	}

	killed, err := startDiagnostics(root, "group unit", now)
	if err != nil {
		t.Fatal(err)
	}
	killed.checkStarted("shipped-workflows")
	// The killed driver never closes its log; Windows cannot remove open files.
	t.Cleanup(func() { _ = killed.log.close() })

	stepSummary := filepath.Join(t.TempDir(), "step-summary.md")
	env := map[string]string{
		diagnosticsDirEnv:     root,
		diagnosticsJobStatus:  "failure",
		"GITHUB_STEP_SUMMARY": stepSummary,
		"GITHUB_JOB":          "unit",
		"RUNNER_OS":           "Linux",
		"ImageOS":             "ubuntu24",
		"ImageVersion":        "20261001.1",
	}
	var out, errOut bytes.Buffer
	if code := runDiagnose(nil, &out, &errOut, func(key string) string { return env[key] }, now); code != 0 {
		t.Fatalf("runDiagnose = %d, stderr %s", code, errOut.String())
	}

	summary, err := os.ReadFile(stepSummary)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"test-failure", "ubuntu24 20261001.1", "`TestOuter/sub_case` in `github.com/goobers/goobers/internal/broken`",
		"outer_test.go:12: want 1, got 2", "shipped-workflows (unfinished)", "| Go | go",
	} {
		if !strings.Contains(string(summary), want) {
			t.Errorf("step summary is missing %q:\n%s", want, summary)
		}
	}
	var recorded diagnosis
	data, err := os.ReadFile(filepath.Join(root, "diagnostics.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	if len(recorded.Runs) != 2 || recorded.Runs[0].FailedCheck != "test" || recorded.Runs[1].Result != runResultRunning {
		t.Fatalf("recorded runs = %+v", recorded.Runs)
	}

	// The job log and annotation carry the class only: flake watch
	// fingerprints test names and FAIL lines found there.
	if !strings.Contains(out.String(), "::notice title=CI failure class%3A test-failure::") {
		t.Fatalf("missing classification notice:\n%s", out.String())
	}
	flakeWatchPatterns := regexp.MustCompile(`\bTest[A-Za-z0-9_]+|--- FAIL|(?m)^FAIL\s`)
	logged := strings.ReplaceAll(out.String(), root, "<root>")
	if match := flakeWatchPatterns.FindString(logged); match != "" {
		t.Fatalf("diagnose log output names %q, which flake watch would fingerprint:\n%s", match, logged)
	}
}

// writingExecutor prints a failing unit suite's output the way go test does.
type writingExecutor struct {
	inner  *fakeExecutor
	stdout io.Writer
}

func (w writingExecutor) run(current check) ([]byte, error) {
	if current.label == "test" {
		_, _ = w.stdout.Write([]byte(goTestFailureOutput + "\n"))
	}
	return w.inner.run(current)
}

func TestCappedLogStopsAtLimit(t *testing.T) {
	t.Parallel()
	file, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	log := &cappedLog{file: file, limit: 8}
	if n, err := log.Write([]byte("0123456789")); n != 10 || err != nil {
		t.Fatalf("Write = %d, %v; the console copy must never see a short write", n, err)
	}
	_, _ = log.Write([]byte("more"))
	_ = log.close()
	data, _ := os.ReadFile(file.Name())
	if !log.truncated() || !strings.HasPrefix(string(data), "01234567\n[ci diagnostics: output.log truncated") ||
		strings.Contains(string(data), "more") {
		t.Fatalf("log = %q", data)
	}
}

func TestDiagnosticsRootOnlyInsideActions(t *testing.T) {
	t.Parallel()
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	if got := diagnosticsRoot(env(map[string]string{"RUNNER_TEMP": "/r"})); got != "" {
		t.Fatalf("outside Actions root = %q, want none", got)
	}
	actions := map[string]string{"GITHUB_ACTIONS": "true", "RUNNER_TEMP": "/r"}
	if got, want := diagnosticsRoot(env(actions)), filepath.Join("/r", diagnosticsRootName); got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
	actions[diagnosticsDirEnv] = "/custom"
	if got := diagnosticsRoot(env(actions)); got != "/custom" {
		t.Fatalf("override root = %q", got)
	}
}

func TestIgnoreWaitDelayKeepsTheExitVerdict(t *testing.T) {
	t.Parallel()
	if _, err := ignoreWaitDelay(nil, exec.ErrWaitDelay); err != nil {
		t.Fatalf("a successful check whose output pipe outlived it must pass: %v", err)
	}
	failure := errors.New("exit status 1")
	if _, err := ignoreWaitDelay(nil, failure); !errors.Is(err, failure) {
		t.Fatalf("a failed check must stay failed: %v", err)
	}
}

// TestCIJobsRecordFailureDiagnostics pins the diagnostics pair in every
// required CI job and in the macOS nightly: the record step must run on every
// outcome and the upload must follow a failure or cancellation, neither able
// to change the job's verdict (#6912).
func TestCIJobsRecordFailureDiagnostics(t *testing.T) {
	t.Parallel()
	ci := loadCIWorkflow(t)
	jobs := map[string]ciJob{}
	for _, name := range ci.Jobs["required-ci"].Needs {
		if name != "scope" {
			jobs["ci.yml "+name] = ci.Jobs[name]
		}
	}
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "workflows", "macos-nightly.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var nightly ciWorkflow
	if err := yaml.Unmarshal(data, &nightly); err != nil {
		t.Fatal(err)
	}
	jobs["macos-nightly.yml runtime"] = nightly.Jobs["runtime"]
	for name, job := range jobs {
		steps := job.Steps
		if len(steps) < 2 {
			t.Errorf("%s: no steps", name)
			continue
		}
		record, upload := steps[len(steps)-2], steps[len(steps)-1]
		if record.Name != "Record CI diagnostics" || record.Run != "go run ./test/ci diagnose" ||
			record.If != "always()" || !record.ContinueOnError ||
			record.Env[diagnosticsJobStatus] != "${{ job.status }}" {
			t.Errorf("%s: second-to-last step must be the always() diagnose step, got %+v", name, record)
		}
		if upload.Name != "Upload CI diagnostics" || !strings.HasPrefix(upload.Uses, "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a") ||
			upload.If != "${{ failure() || cancelled() }}" || !upload.ContinueOnError ||
			upload.with("name") != "ci-diagnostics-${{ github.job }}-${{ strategy.job-index }}" ||
			upload.with("path") != "${{ runner.temp }}/"+diagnosticsRootName {
			t.Errorf("%s: last step must upload the diagnostics on failure, got %+v", name, upload)
		}
	}
}
