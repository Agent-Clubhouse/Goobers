package main

// CI failure diagnostics (#6912).
//
// In GitHub Actions every CI driver invocation records its own output and
// progress under the diagnostics root ($RUNNER_TEMP/goobers-ci-diagnostics, or
// GOOBERS_CI_DIAGNOSTICS_DIR): output.log is a copy of everything the driver
// and its checks printed, and state.json says which check is running, which
// finished, and how the invocation ended. state.json is rewritten as each check
// starts and ends, so a driver killed by a step timeout or a cancellation
// leaves a record that still says "running".
//
// `go run ./test/ci diagnose` runs afterwards as an always() step. It records
// the job's runner and toolchain, classifies a failed job as a test failure, a
// non-test check failure, or an infrastructure failure (timeout, cancellation,
// setup), extracts the failing tests and packages with their output, and writes
// all of it to the job summary and to files that the workflow uploads as the
// ci-diagnostics artifact. It never changes a job's verdict.
//
// Flake watch fingerprints test failures from job logs and from annotations
// whose text names a test. Everything that names a test therefore goes only to
// the step summary and the artifact; the log line and the notice annotation
// this command prints carry the classification alone, so no failure is
// fingerprinted twice.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	diagnosticsDirEnv     = "GOOBERS_CI_DIAGNOSTICS_DIR"
	diagnosticsJobStatus  = "GOOBERS_CI_JOB_STATUS"
	diagnosticsRootName   = "goobers-ci-diagnostics"
	diagnosticsStateFile  = "state.json"
	diagnosticsLogFile    = "output.log"
	diagnosticsSchema     = "goobers.dev/ci-diagnostics/v1"
	diagnosticsLogLimit   = 64 << 20
	diagnosticsWaitDelay  = 30 * time.Second
	excerptLinesPerTest   = 60
	excerptTailLines      = 80
	excerptMaxBytes       = 48 << 10
	summaryListLimit      = 50
	runResultRunning      = "running"
	runResultPassed       = "passed"
	runResultFailed       = "failed"
	categoryTest          = "test"
	categoryCheck         = "check"
	categoryInfra         = "infrastructure"
	categoryOutsideDriver = "outside-driver"
)

// diagnosticsRoot is where driver invocations record their output: the
// explicit override, else a directory under RUNNER_TEMP inside GitHub Actions.
// Outside CI it is empty and nothing is recorded.
func diagnosticsRoot(getenv func(string) string) string {
	if dir := strings.TrimSpace(getenv(diagnosticsDirEnv)); dir != "" {
		return dir
	}
	if getenv("GITHUB_ACTIONS") != "true" || strings.TrimSpace(getenv("RUNNER_TEMP")) == "" {
		return ""
	}
	return filepath.Join(getenv("RUNNER_TEMP"), diagnosticsRootName)
}

type checkTiming struct {
	Label    string    `json:"label"`
	Started  time.Time `json:"started"`
	Seconds  float64   `json:"seconds"`
	Finished bool      `json:"finished"`
	Failed   bool      `json:"failed,omitempty"`
}

// driverRun is one CI driver invocation's state.json.
type driverRun struct {
	Schema       string        `json:"schema"`
	Mode         string        `json:"mode"`
	Started      time.Time     `json:"started"`
	Finished     *time.Time    `json:"finished,omitempty"`
	Result       string        `json:"result"`
	FailedCheck  string        `json:"failed_check,omitempty"`
	Error        string        `json:"error,omitempty"`
	LogTruncated bool          `json:"log_truncated,omitempty"`
	Checks       []checkTiming `json:"checks"`

	dir string
}

// diagnosticsRecorder records one driver invocation. All methods are safe on a
// nil receiver, which is how an unrecorded (local) run behaves.
type diagnosticsRecorder struct {
	mu    sync.Mutex
	dir   string
	now   func() time.Time
	state driverRun
	log   *cappedLog
}

func startDiagnostics(root, mode string, now func() time.Time) (*diagnosticsRecorder, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	dir, err := newRunDir(root, mode)
	if err != nil {
		return nil, err
	}
	file, err := os.Create(filepath.Join(dir, diagnosticsLogFile))
	if err != nil {
		return nil, err
	}
	recorder := &diagnosticsRecorder{
		dir: dir,
		now: now,
		log: &cappedLog{file: file, limit: diagnosticsLogLimit},
		state: driverRun{
			Schema:  diagnosticsSchema,
			Mode:    mode,
			Started: now().UTC(),
			Result:  runResultRunning,
			Checks:  []checkTiming{},
		},
	}
	if err := recorder.saveLocked(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return recorder, nil
}

// newRunDir creates a fresh directory per invocation, so a job that runs the
// driver twice (macOS nightly: unit, then shipped) keeps both records.
func newRunDir(root, mode string) (string, error) {
	base := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, mode)
	for attempt := 1; attempt < 1000; attempt++ {
		name := base
		if attempt > 1 {
			name = fmt.Sprintf("%s-%d", base, attempt)
		}
		dir := filepath.Join(root, name)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free diagnostics directory for %q under %s", mode, root)
}

// writers tees the driver's output into output.log.
func (r *diagnosticsRecorder) writers(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	if r == nil {
		return stdout, stderr
	}
	return io.MultiWriter(stdout, r.log), io.MultiWriter(stderr, r.log)
}

func (r *diagnosticsRecorder) checkStarted(label string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Checks = append(r.state.Checks, checkTiming{Label: label, Started: r.now().UTC()})
	_ = r.saveLocked()
}

func (r *diagnosticsRecorder) checkFinished(label string, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.state.Checks) - 1; i >= 0; i-- {
		current := &r.state.Checks[i]
		if current.Label == label && !current.Finished {
			current.Finished = true
			current.Failed = err != nil
			current.Seconds = r.now().Sub(current.Started).Seconds()
			break
		}
	}
	_ = r.saveLocked()
}

// finish records how the invocation ended. The driver stops at its first
// failing check, so the failed check is the last one it started.
func (r *diagnosticsRecorder) finish(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	finished := r.now().UTC()
	r.state.Finished = &finished
	r.state.Result = runResultPassed
	if err != nil {
		r.state.Result = runResultFailed
		r.state.Error = err.Error()
		if count := len(r.state.Checks); count > 0 {
			last := &r.state.Checks[count-1]
			last.Failed = true
			r.state.FailedCheck = last.Label
		}
	}
	r.state.LogTruncated = r.log.truncated()
	_ = r.saveLocked()
	_ = r.log.close()
}

func (r *diagnosticsRecorder) saveLocked() error {
	data, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(r.dir, diagnosticsStateFile), append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// cappedLog stops copying output into the log file past its limit; the
// console copy is unaffected.
type cappedLog struct {
	mu      sync.Mutex
	file    *os.File
	limit   int64
	written int64
	capped  bool
}

func (c *cappedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil || c.capped {
		return len(p), nil
	}
	room := c.limit - c.written
	chunk := p
	if int64(len(chunk)) > room {
		chunk = chunk[:room]
		c.capped = true
	}
	written, _ := c.file.Write(chunk)
	c.written += int64(written)
	if c.capped {
		_, _ = fmt.Fprintf(c.file, "\n[ci diagnostics: output.log truncated at %d bytes]\n", c.limit)
	}
	return len(p), nil
}

func (c *cappedLog) truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capped
}

func (c *cappedLog) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return nil
	}
	err := c.file.Close()
	c.file = nil
	return err
}

// recordingExecutor reports each executed check to the recorder.
type recordingExecutor struct {
	inner    executor
	recorder *diagnosticsRecorder
}

func (e recordingExecutor) run(current check) ([]byte, error) {
	e.recorder.checkStarted(current.label)
	output, err := e.inner.run(current)
	e.recorder.checkFinished(current.label, err)
	return output, err
}

// --- Failure extraction ---------------------------------------------------

var (
	failTestLine     = regexp.MustCompile(`^(\s*)--- FAIL: (\S+)(?: \(|$)`)
	testHeaderLine   = regexp.MustCompile(`^(\s*)--- (?:FAIL|PASS|SKIP): `)
	failPackageLine  = regexp.MustCompile(`^FAIL\s+(\S+)\s+(?:\[([a-z ]+)\]|[0-9.]+s)\s*$`)
	okPackageLine    = regexp.MustCompile(`^ok\s+\S+`)
	testTimeoutLine  = regexp.MustCompile(`^panic: test timed out after (\S+)`)
	driverMarkerLine = regexp.MustCompile(`^(?:==>|<==) `)
	// verboseMarkerLine is the `=== RUN|PAUSE|CONT|NAME <test>` line verbose
	// go test prints when a test starts or resumes printing.
	verboseMarkerLine = regexp.MustCompile(`^=== (RUN|PAUSE|CONT|NAME)\s+(\S+)`)
)

type failedTest struct {
	Package string `json:"package,omitempty"`
	Name    string `json:"name"`
}

type failedPackage struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type failureReport struct {
	Tests       []failedTest    `json:"tests,omitempty"`
	Packages    []failedPackage `json:"packages,omitempty"`
	TestTimeout string          `json:"test_timeout,omitempty"`
	Excerpt     []string        `json:"-"`
}

// extractFailures reads go test output (plain, or the text test/testtiming
// relays from -json) for failing tests and packages. A failing test's
// `--- FAIL:` line comes before its package's FAIL line, so tests are
// attributed to the next FAIL package line.
func extractFailures(lines []string) failureReport {
	var report failureReport
	var pending []failedTest
	seenTests := map[string]bool{}
	seenPackages := map[string]bool{}
	for index, line := range lines {
		if match := failTestLine.FindStringSubmatch(line); match != nil {
			pending = append(pending, failedTest{Name: match[2]})
			report.Excerpt = append(report.Excerpt, failureBlock(lines, index, len(match[1]))...)
			continue
		}
		if match := testTimeoutLine.FindStringSubmatch(line); match != nil && report.TestTimeout == "" {
			report.TestTimeout = match[1]
			report.Excerpt = append(report.Excerpt, failureBlock(lines, index, -1)...)
			continue
		}
		match := failPackageLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		reason := match[2]
		if reason == "" {
			reason = "tests failed"
			if len(pending) == 0 {
				reason = "package failed"
			}
		}
		for _, test := range pending {
			test.Package = match[1]
			if key := test.Package + "\x00" + test.Name; !seenTests[key] {
				seenTests[key] = true
				report.Tests = append(report.Tests, test)
			}
		}
		pending = nil
		if !seenPackages[match[1]] {
			seenPackages[match[1]] = true
			report.Packages = append(report.Packages, failedPackage{Name: match[1], Reason: reason})
		}
	}
	for _, test := range pending {
		if key := "\x00" + test.Name; !seenTests[key] {
			seenTests[key] = true
			report.Tests = append(report.Tests, test)
		}
	}
	return report
}

// failureBlock is the output of one failing test. Verbose output (-v, or the
// -json stream test/testtiming relays) prints a test's log lines before its
// `--- FAIL:` line, between `=== RUN`/`=== CONT`/`=== NAME` markers, so those
// are collected first by verboseOutput. Then come the header line and what
// follows, up to the next test header at the same or an outer level, a
// verbose marker, the package result, or a driver marker. indent < 0 (a
// timeout panic) runs to the package result.
func failureBlock(lines []string, start, indent int) []string {
	var block []string
	if indent >= 0 {
		if match := failTestLine.FindStringSubmatch(lines[start]); match != nil {
			block = verboseOutput(lines, start, match[2])
		}
	}
	block = append(block, lines[start])
	limit := len(block) + excerptLinesPerTest
	for index := start + 1; index < len(lines) && len(block) < limit; index++ {
		line := lines[index]
		if failPackageLine.MatchString(line) || okPackageLine.MatchString(line) ||
			driverMarkerLine.MatchString(line) || verboseMarkerLine.MatchString(line) ||
			strings.TrimSpace(line) == "FAIL" {
			break
		}
		if match := testHeaderLine.FindStringSubmatch(line); match != nil && indent >= 0 && len(match[1]) <= indent {
			break
		}
		block = append(block, line)
	}
	return append(block, "")
}

// verboseOutput is what test name (and its subtests) printed in verbose output
// before its `--- FAIL:` line at end: the lines after its last `=== RUN name`
// marker that belong to it, skipping segments other (parallel) tests printed.
// The last excerptLinesPerTest lines are kept, as failures are usually
// reported last. Non-verbose output has no markers and yields nothing.
func verboseOutput(lines []string, end int, name string) []string {
	start := -1
	for index := end - 1; index >= 0; index-- {
		line := lines[index]
		if failPackageLine.MatchString(line) || okPackageLine.MatchString(line) || driverMarkerLine.MatchString(line) {
			break
		}
		if match := verboseMarkerLine.FindStringSubmatch(line); match != nil && match[1] == "RUN" && match[2] == name {
			start = index
			break
		}
	}
	if start < 0 {
		return nil
	}
	var owned []string
	owner := ""
	for index := start; index < end; index++ {
		line := lines[index]
		if match := verboseMarkerLine.FindStringSubmatch(line); match != nil {
			owner = match[2]
			continue
		}
		if owner == name || strings.HasPrefix(owner, name+"/") {
			owned = append(owned, line)
		}
	}
	if len(owned) > excerptLinesPerTest {
		owned = append([]string{fmt.Sprintf("... (%d earlier lines omitted)", len(owned)-excerptLinesPerTest)},
			owned[len(owned)-excerptLinesPerTest:]...)
	}
	return owned
}

// --- Environment ------------------------------------------------------------

type environmentRecord struct {
	Job               string    `json:"job"`
	Workflow          string    `json:"workflow,omitempty"`
	RunID             string    `json:"run_id,omitempty"`
	RunAttempt        string    `json:"run_attempt,omitempty"`
	Event             string    `json:"event,omitempty"`
	Ref               string    `json:"ref,omitempty"`
	SHA               string    `json:"sha,omitempty"`
	RunnerOS          string    `json:"runner_os,omitempty"`
	RunnerArch        string    `json:"runner_arch,omitempty"`
	RunnerName        string    `json:"runner_name,omitempty"`
	RunnerEnvironment string    `json:"runner_environment,omitempty"`
	ImageOS           string    `json:"image_os,omitempty"`
	ImageVersion      string    `json:"image_version,omitempty"`
	GoVersion         string    `json:"go_version"`
	GOOS              string    `json:"goos"`
	GOARCH            string    `json:"goarch"`
	CPUs              int       `json:"cpus"`
	RecordedAt        time.Time `json:"recorded_at"`
}

func collectEnvironment(getenv func(string) string, now time.Time) environmentRecord {
	return environmentRecord{
		Job:               getenv("GITHUB_JOB"),
		Workflow:          getenv("GITHUB_WORKFLOW"),
		RunID:             getenv("GITHUB_RUN_ID"),
		RunAttempt:        getenv("GITHUB_RUN_ATTEMPT"),
		Event:             getenv("GITHUB_EVENT_NAME"),
		Ref:               getenv("GITHUB_REF"),
		SHA:               getenv("GITHUB_SHA"),
		RunnerOS:          getenv("RUNNER_OS"),
		RunnerArch:        getenv("RUNNER_ARCH"),
		RunnerName:        getenv("RUNNER_NAME"),
		RunnerEnvironment: getenv("RUNNER_ENVIRONMENT"),
		ImageOS:           getenv("ImageOS"),
		ImageVersion:      getenv("ImageVersion"),
		GoVersion:         runtime.Version(),
		GOOS:              runtime.GOOS,
		GOARCH:            runtime.GOARCH,
		CPUs:              runtime.NumCPU(),
		RecordedAt:        now.UTC(),
	}
}

// --- Classification ---------------------------------------------------------

type classification struct {
	Class    string `json:"class"`
	Category string `json:"category"`
	Detail   string `json:"detail"`
}

type runDiagnosis struct {
	Run    driverRun
	Report failureReport
	Tail   []string
}

// classifyJob tells test failures from non-test check failures and from
// infrastructure failures. jobStatus is the workflow's job.status (success,
// failure, or cancelled) as of the diagnose step.
func classifyJob(jobStatus string, runs []runDiagnosis, now time.Time) classification {
	if jobStatus == "success" {
		return classification{Class: "passed", Category: "none", Detail: "the job passed"}
	}
	for _, current := range runs {
		if current.Run.Result == runResultFailed {
			return classifyFailedRun(current)
		}
	}
	for _, current := range runs {
		if current.Run.Result == runResultRunning {
			return classifyInterruptedRun(jobStatus, current.Run, now)
		}
	}
	switch {
	case jobStatus == "cancelled":
		return classification{Class: "cancelled", Category: categoryInfra,
			Detail: "the job was cancelled (run cancelled or superseded, or the job timed out) while no CI driver check was running"}
	case len(runs) == 0:
		return classification{Class: "outside-driver-failure", Category: categoryOutsideDriver,
			Detail: "a step failed and the CI driver never ran: setup (checkout, toolchain, module download) or another step before or instead of it; see the failed step"}
	default:
		return classification{Class: "post-driver-failure", Category: categoryOutsideDriver,
			Detail: "every CI driver check passed; a later step of the job failed; see the failed step"}
	}
}

func classifyFailedRun(current runDiagnosis) classification {
	report := current.Report
	check := current.Run.FailedCheck
	if check == "" {
		check = "(none)"
	}
	switch {
	case report.TestTimeout != "":
		return classification{Class: "test-timeout", Category: categoryTest,
			Detail: fmt.Sprintf("check %s: a test exceeded the go test -timeout of %s (hung or too slow)", check, report.TestTimeout)}
	case len(report.Tests) > 0:
		return classification{Class: "test-failure", Category: categoryTest,
			Detail: fmt.Sprintf("check %s: %d failing test(s) in %d package(s)", check, len(report.Tests), len(report.Packages))}
	case hasPackageReason(report.Packages, "build failed", "setup failed"):
		return classification{Class: "build-failure", Category: categoryCheck,
			Detail: fmt.Sprintf("check %s: a package did not compile", check)}
	case len(report.Packages) > 0:
		return classification{Class: "test-failure", Category: categoryTest,
			Detail: fmt.Sprintf("check %s: %d package(s) failed without a failing test (panic, TestMain, or exit)", check, len(report.Packages))}
	default:
		return classification{Class: "check-failure", Category: categoryCheck,
			Detail: fmt.Sprintf("check %s failed without go test failures (lint, format, policy, build, or tool error)", check)}
	}
}

func classifyInterruptedRun(jobStatus string, run driverRun, now time.Time) classification {
	check, running := "(between checks)", time.Duration(0)
	if count := len(run.Checks); count > 0 && !run.Checks[count-1].Finished {
		check = run.Checks[count-1].Label
		running = now.Sub(run.Checks[count-1].Started).Round(time.Second)
	}
	if jobStatus == "cancelled" {
		return classification{Class: "cancelled", Category: categoryInfra,
			Detail: fmt.Sprintf("the job was cancelled (run cancelled or superseded, or the job timed out) while check %s had run for %s", check, running)}
	}
	return classification{Class: "step-timeout", Category: categoryInfra,
		Detail: fmt.Sprintf("the CI driver was stopped before it finished (step timeout or runner shutdown) while check %s had run for %s", check, running)}
}

func hasPackageReason(packages []failedPackage, reasons ...string) bool {
	for _, current := range packages {
		for _, reason := range reasons {
			if current.Reason == reason {
				return true
			}
		}
	}
	return false
}

// --- diagnose command --------------------------------------------------------

type diagnosis struct {
	Schema         string            `json:"schema"`
	JobStatus      string            `json:"job_status"`
	Classification classification    `json:"classification"`
	Environment    environmentRecord `json:"environment"`
	Runs           []diagnosedRun    `json:"runs"`
}

type diagnosedRun struct {
	driverRun
	Failures failureReport `json:"failures"`
}

func runDiagnose(args []string, stdout, stderr io.Writer, getenv func(string) string, now func() time.Time) int {
	if len(args) != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/ci diagnose")
		return 2
	}
	root := diagnosticsRoot(getenv)
	if root == "" {
		_, _ = fmt.Fprintln(stdout, "ci diagnostics: not in GitHub Actions and "+diagnosticsDirEnv+" is unset; nothing to record")
		return 0
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "ci diagnostics: %v\n", err)
		return 1
	}
	runs, err := loadRuns(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ci diagnostics: %v\n", err)
		return 1
	}
	jobStatus := strings.TrimSpace(getenv(diagnosticsJobStatus))
	if jobStatus == "" {
		jobStatus = "unknown"
	}
	at := now()
	result := diagnosis{
		Schema:         diagnosticsSchema,
		JobStatus:      jobStatus,
		Classification: classifyJob(jobStatus, runs, at),
		Environment:    collectEnvironment(getenv, at),
	}
	for _, current := range runs {
		result.Runs = append(result.Runs, diagnosedRun{driverRun: current.Run, Failures: current.Report})
	}
	summary := renderSummary(result, runs)
	if err := writeDiagnosis(root, result, summary, getenv("GITHUB_STEP_SUMMARY")); err != nil {
		_, _ = fmt.Fprintf(stderr, "ci diagnostics: %v\n", err)
		return 1
	}
	printDiagnosisLog(stdout, result, root)
	return 0
}

func loadRuns(root string) ([]runDiagnosis, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var runs []runDiagnosis
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, diagnosticsStateFile))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var run driverRun
		if err := json.Unmarshal(data, &run); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, diagnosticsStateFile), err)
		}
		run.dir = dir
		current := runDiagnosis{Run: run}
		if run.Result != runResultPassed {
			lines := readLogLines(filepath.Join(dir, diagnosticsLogFile))
			current.Report = extractFailures(lines)
			current.Tail = lastLines(lines, excerptTailLines)
		}
		runs = append(runs, current)
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Run.Started.Before(runs[j].Run.Started) })
	return runs, nil
}

func readLogLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

func lastLines(lines []string, count int) []string {
	if len(lines) <= count {
		return lines
	}
	return lines[len(lines)-count:]
}

func writeDiagnosis(root string, result diagnosis, summary, stepSummary string) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "diagnostics.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "summary.md"), []byte(summary), 0o644); err != nil {
		return err
	}
	if stepSummary == "" {
		return nil
	}
	file, err := os.OpenFile(stepSummary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, summary)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

// printDiagnosisLog prints the classification to the job log and, for a job
// that did not pass, as a notice annotation. Neither names a test (see the
// file comment): test names stay in the summary and artifact.
func printDiagnosisLog(stdout io.Writer, result diagnosis, root string) {
	c := result.Classification
	_, _ = fmt.Fprintf(stdout, "ci diagnostics: job %s status=%s class=%s category=%s\n",
		result.Environment.Job, result.JobStatus, c.Class, c.Category)
	_, _ = fmt.Fprintf(stdout, "ci diagnostics: wrote %s and %s\n",
		filepath.Join(root, "summary.md"), filepath.Join(root, "diagnostics.json"))
	if result.JobStatus == "success" {
		return
	}
	message := fmt.Sprintf("%s (%s): %s. Failing tests, output, and runner details are in the job summary and the ci-diagnostics artifact.",
		c.Class, c.Category, annotationSafe(c.Detail))
	_, _ = fmt.Fprintf(stdout, "::notice title=%s::%s\n",
		escapeWorkflowProperty("CI failure class: "+c.Class), escapeWorkflowData(message))
}

// annotationSafe drops anything shaped like a Go test name, so flake watch
// never fingerprints this annotation.
func annotationSafe(text string) string {
	return testNameShape.ReplaceAllString(text, "<test>")
}

var testNameShape = regexp.MustCompile(`\bTest[A-Za-z0-9_]+(?:/[A-Za-z0-9_.-]+)*`)

func escapeWorkflowData(value string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(value)
}

func escapeWorkflowProperty(value string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(value)
}

// --- Markdown ---------------------------------------------------------------

func renderSummary(result diagnosis, runs []runDiagnosis) string {
	var b strings.Builder
	env := result.Environment
	c := result.Classification
	fmt.Fprintf(&b, "### CI diagnostics: %s — %s\n\n", markdownCell(orDash(env.Job)), c.Class)
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Job status | %s |\n", markdownCell(result.JobStatus))
	fmt.Fprintf(&b, "| Classification | **%s** (%s): %s |\n", c.Class, c.Category, markdownCell(c.Detail))
	fmt.Fprintf(&b, "| Runner | %s %s, image %s %s, %s (%s) |\n",
		markdownCell(orDash(env.RunnerOS)), markdownCell(orDash(env.RunnerArch)), markdownCell(orDash(env.ImageOS)),
		markdownCell(orDash(env.ImageVersion)), markdownCell(orDash(env.RunnerName)), markdownCell(orDash(env.RunnerEnvironment)))
	fmt.Fprintf(&b, "| Go | %s %s/%s, %d CPUs |\n", env.GoVersion, env.GOOS, env.GOARCH, env.CPUs)
	fmt.Fprintf(&b, "| Run | %s attempt %s, %s on %s @ %s |\n", markdownCell(orDash(env.RunID)), markdownCell(orDash(env.RunAttempt)),
		markdownCell(orDash(env.Event)), markdownCell(orDash(env.Ref)), markdownCell(orDash(shortSHA(env.SHA))))
	fmt.Fprintf(&b, "| Recorded | %s |\n\n", env.RecordedAt.Format(time.RFC3339))
	if len(runs) > 0 {
		renderRuns(&b, runs, env.RecordedAt)
	}
	for _, current := range runs {
		if current.Run.Result != runResultPassed {
			renderFailures(&b, current)
		}
	}
	return b.String()
}

func renderRuns(b *strings.Builder, runs []runDiagnosis, now time.Time) {
	b.WriteString("| CI driver | Result | Started | Duration | Failed or unfinished check | Slowest checks |\n|---|---|---|---|---|---|\n")
	for _, current := range runs {
		run := current.Run
		end := now
		if run.Finished != nil {
			end = *run.Finished
		}
		check := run.FailedCheck
		if count := len(run.Checks); run.Result == runResultRunning && count > 0 && !run.Checks[count-1].Finished {
			check = run.Checks[count-1].Label + " (unfinished)"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", markdownCell(run.Mode), run.Result,
			run.Started.Format(time.RFC3339), end.Sub(run.Started).Round(time.Second),
			markdownCell(orDash(check)), markdownCell(slowestChecks(run.Checks, 3)))
	}
	b.WriteString("\n")
}

func slowestChecks(checks []checkTiming, count int) string {
	finished := make([]checkTiming, 0, len(checks))
	for _, current := range checks {
		if current.Finished {
			finished = append(finished, current)
		}
	}
	sort.SliceStable(finished, func(i, j int) bool { return finished[i].Seconds > finished[j].Seconds })
	parts := []string{}
	for index := 0; index < len(finished) && index < count; index++ {
		parts = append(parts, fmt.Sprintf("%s %s", finished[index].Label,
			(time.Duration(finished[index].Seconds*float64(time.Second))).Round(time.Second)))
	}
	return orDash(strings.Join(parts, ", "))
}

func renderFailures(b *strings.Builder, current runDiagnosis) {
	report := current.Report
	fmt.Fprintf(b, "#### %s: %s\n\n", markdownCell(current.Run.Mode), current.Run.Result)
	if current.Run.Error != "" {
		fmt.Fprintf(b, "Driver error: `%s`\n\n", strings.ReplaceAll(current.Run.Error, "`", "'"))
	}
	if len(report.Tests) > 0 {
		fmt.Fprintf(b, "Failing tests (%d):\n\n", len(report.Tests))
		for index, test := range report.Tests {
			if index == summaryListLimit {
				fmt.Fprintf(b, "- … %d more in the artifact's diagnostics.json\n", len(report.Tests)-summaryListLimit)
				break
			}
			fmt.Fprintf(b, "- `%s` in `%s`\n", test.Name, orDash(test.Package))
		}
		b.WriteString("\n")
	}
	if len(report.Packages) > 0 {
		fmt.Fprintf(b, "Failing packages (%d):\n\n", len(report.Packages))
		for index, pkg := range report.Packages {
			if index == summaryListLimit {
				fmt.Fprintf(b, "- … %d more in the artifact's diagnostics.json\n", len(report.Packages)-summaryListLimit)
				break
			}
			fmt.Fprintf(b, "- `%s` (%s)\n", pkg.Name, pkg.Reason)
		}
		b.WriteString("\n")
	}
	if len(report.Excerpt) > 0 {
		writeCodeBlock(b, "Failure output", report.Excerpt)
	}
	writeCodeBlock(b, fmt.Sprintf("Last %d log lines", len(current.Tail)), current.Tail)
	if current.Run.LogTruncated {
		b.WriteString("output.log was truncated; the console log has the rest.\n\n")
	}
}

func writeCodeBlock(b *strings.Builder, title string, lines []string) {
	text := strings.Join(lines, "\n")
	if len(text) > excerptMaxBytes {
		text = text[:excerptMaxBytes] + "\n[… truncated; see output.log in the ci-diagnostics artifact]"
	}
	fence := strings.Repeat("`", max(3, longestBacktickRun(text)+1))
	fmt.Fprintf(b, "<details><summary>%s</summary>\n\n%stext\n%s\n%s\n\n</details>\n\n", title, fence, text, fence)
}

func longestBacktickRun(text string) int {
	longest, current := 0, 0
	for _, r := range text {
		if r == '`' {
			current++
			longest = max(longest, current)
			continue
		}
		current = 0
	}
	return longest
}

func markdownCell(value string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace(value)
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// driverMode names a driver invocation in its record.
func driverMode(args []string) string {
	if len(args) == 0 {
		return "merge"
	}
	if args[0] == "full" {
		return "full"
	}
	return strings.Join(args, " ")
}
