// Command stress repeatedly runs explicitly enrolled packages under the race detector.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	stressCount      = 20
	reportSchema     = "goobers.dev/stress/v1"
	failureTextLimit = 64 * 1024
)

type options struct {
	goCommand   string
	packageList string
	outputDir   string
	seed        int64
}

type runMetadata struct {
	RunID      string    `json:"run_id"`
	RunAttempt string    `json:"run_attempt"`
	Trigger    string    `json:"trigger"`
	Repository string    `json:"repository,omitempty"`
	Ref        string    `json:"ref,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	Workflow   string    `json:"workflow,omitempty"`
	Job        string    `json:"job,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	URL        string    `json:"url,omitempty"`
	RunnerOS   string    `json:"runner_os"`
	RunnerArch string    `json:"runner_arch"`
	GoVersion  string    `json:"go_version"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

type summaryReport struct {
	SchemaVersion string          `json:"schema_version"`
	Status        string          `json:"status"`
	Race          bool            `json:"race"`
	Count         int             `json:"count"`
	Seed          int64           `json:"seed"`
	Run           runMetadata     `json:"run"`
	FailuresFile  string          `json:"failures_file"`
	Packages      []packageResult `json:"packages"`
}

type packageResult struct {
	Package            string    `json:"package"`
	Status             string    `json:"status"`
	Race               bool      `json:"race"`
	Count              int       `json:"count"`
	Seed               int64     `json:"seed"`
	StartedAt          time.Time `json:"started_at"`
	FinishedAt         time.Time `json:"finished_at"`
	WallDurationSecs   float64   `json:"wall_duration_seconds"`
	TestElapsedSecs    float64   `json:"test_elapsed_seconds"`
	EventLog           string    `json:"event_log"`
	StderrLog          string    `json:"stderr_log"`
	StructuredFailures int       `json:"structured_failures"`
}

type failuresReport struct {
	SchemaVersion string        `json:"schema_version"`
	Run           runMetadata   `json:"run"`
	Failures      []testFailure `json:"failures"`
}

type testFailure struct {
	Fingerprint          string    `json:"fingerprint"`
	Package              string    `json:"package"`
	Test                 string    `json:"test"`
	FailureText          string    `json:"failure_text"`
	FailureTextTruncated bool      `json:"failure_text_truncated"`
	FirstSeenRun         string    `json:"first_seen_run"`
	LastSeenRun          string    `json:"last_seen_run"`
	FirstSeenAt          time.Time `json:"first_seen_at"`
	LastSeenAt           time.Time `json:"last_seen_at"`
	Occurrences          int       `json:"occurrences"`
}

type testEvent struct {
	Time    time.Time `json:"Time"`
	Action  string    `json:"Action"`
	Package string    `json:"Package"`
	Test    string    `json:"Test"`
	Elapsed float64   `json:"Elapsed"`
	Output  string    `json:"Output"`
}

type commandFactory func(context.Context, string, ...string) *exec.Cmd

type processRunner struct {
	command   commandFactory
	goCommand string
	outputDir string
	runID     string
	count     int
	seed      int64
	stdout    io.Writer
	stderr    io.Writer
	now       func() time.Time
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, time.Now, exec.CommandContext))
}

func run(
	args []string,
	stdout, stderr io.Writer,
	getenv func(string) string,
	now func() time.Time,
	command commandFactory,
) int {
	opts, err := parseOptions(args, stderr, getenv)
	if err != nil {
		return 2
	}

	packagesFile, err := os.Open(opts.packageList)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "stress: open package list: %v\n", err)
		return 2
	}
	packages, loadErr := loadPackages(packagesFile)
	closeErr := packagesFile.Close()
	if err := errors.Join(loadErr, closeErr); err != nil {
		_, _ = fmt.Fprintf(stderr, "stress: load package list: %v\n", err)
		return 2
	}

	started := now().UTC()
	if opts.seed == 0 {
		opts.seed = started.UnixNano()
	}
	metadata := metadataFromEnvironment(getenv, started)
	runner := processRunner{
		command:   command,
		goCommand: opts.goCommand,
		outputDir: opts.outputDir,
		runID:     metadata.RunID,
		count:     stressCount,
		seed:      opts.seed,
		stdout:    stdout,
		stderr:    stderr,
		now:       now,
	}

	summary, failures, executeErr := executeStress(context.Background(), runner, packages)
	metadata.FinishedAt = now().UTC()
	summary.Run = metadata
	failures.Run = metadata
	if err := writeReports(opts.outputDir, summary, failures); err != nil {
		_, _ = fmt.Fprintf(stderr, "stress: write reports: %v\n", err)
		return 2
	}

	_, _ = fmt.Fprintf(stdout, "stress: %s (%d package(s)); reports: %s\n",
		summary.Status, len(summary.Packages), opts.outputDir)
	if executeErr != nil {
		_, _ = fmt.Fprintf(stderr, "stress: execution error: %v\n", executeErr)
		return 1
	}
	if summary.Status != "pass" {
		return 1
	}
	return 0
}

func parseOptions(args []string, stderr io.Writer, getenv func(string) string) (options, error) {
	flags := flag.NewFlagSet("stress", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts options
	flags.StringVar(&opts.goCommand, "go", envOrDefault(getenv, "GO", "go"), "Go toolchain binary")
	flags.StringVar(&opts.packageList, "packages", "test/stress/packages.txt", "checked-in package enrollment list")
	flags.StringVar(&opts.outputDir, "output", "stress-results", "artifact output directory")
	flags.Int64Var(&opts.seed, "seed", 0, "test shuffle seed (zero chooses and records a seed)")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/stress [-go binary] [-packages file] [-output dir] [-seed n]")
		return options{}, errors.New("unexpected positional arguments")
	}
	if strings.TrimSpace(opts.goCommand) == "" || strings.TrimSpace(opts.packageList) == "" ||
		strings.TrimSpace(opts.outputDir) == "" || opts.seed < 0 {
		_, _ = fmt.Fprintln(stderr, "stress: -go, -packages, and -output must be non-empty; -seed must be non-negative")
		return options{}, errors.New("invalid options")
	}
	return opts, nil
}

func loadPackages(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	seen := make(map[string]struct{})
	var packages []string
	for line := 1; scanner.Scan(); line++ {
		value := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if value == "" {
			continue
		}
		if !strings.HasPrefix(value, "./") || strings.ContainsAny(value, " \t") {
			return nil, fmt.Errorf("line %d: package must be a relative Go package pattern, got %q", line, value)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("line %d: duplicate package %q", line, value)
		}
		seen[value] = struct{}{}
		packages = append(packages, value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(packages) == 0 {
		return nil, errors.New("package list is empty")
	}
	return packages, nil
}

func executeStress(ctx context.Context, runner processRunner, packages []string) (summaryReport, failuresReport, error) {
	summary := summaryReport{
		SchemaVersion: reportSchema,
		Status:        "pass",
		Race:          true,
		Count:         runner.count,
		Seed:          runner.seed,
		FailuresFile:  "failures.json",
		Packages:      make([]packageResult, 0, len(packages)),
	}
	failures := failuresReport{
		SchemaVersion: reportSchema,
		Failures:      make([]testFailure, 0),
	}
	if err := os.MkdirAll(filepath.Join(runner.outputDir, "packages"), 0o755); err != nil {
		summary.Status = "fail"
		return summary, failures, err
	}

	var executionErr error
	for _, pkg := range packages {
		result, packageFailures, err := runner.runPackage(ctx, pkg)
		summary.Packages = append(summary.Packages, result)
		failures.Failures = append(failures.Failures, packageFailures...)
		if result.Status != "pass" {
			summary.Status = "fail"
		}
		if err != nil {
			executionErr = errors.Join(executionErr, fmt.Errorf("%s: %w", pkg, err))
		}
	}
	return summary, failures, executionErr
}

func (r processRunner) runPackage(ctx context.Context, pkg string) (packageResult, []testFailure, error) {
	started := r.now().UTC()
	base := artifactBase(pkg)
	eventRel := filepath.ToSlash(filepath.Join("packages", base+".jsonl"))
	stderrRel := filepath.ToSlash(filepath.Join("packages", base+".stderr.txt"))
	result := packageResult{
		Package:   pkg,
		Status:    "fail",
		Race:      true,
		Count:     r.count,
		Seed:      r.seed,
		StartedAt: started,
		EventLog:  eventRel,
		StderrLog: stderrRel,
	}

	if err := os.MkdirAll(filepath.Join(r.outputDir, "packages"), 0o755); err != nil {
		return finishPackageResult(result, started, r.now(), 0, 0), nil, err
	}
	eventFile, err := os.Create(filepath.Join(r.outputDir, filepath.FromSlash(eventRel)))
	if err != nil {
		return finishPackageResult(result, started, r.now(), 0, 0), nil, err
	}
	stderrFile, err := os.Create(filepath.Join(r.outputDir, filepath.FromSlash(stderrRel)))
	if err != nil {
		_ = eventFile.Close()
		return finishPackageResult(result, started, r.now(), 0, 0), nil, err
	}

	var stderrText strings.Builder
	command := r.command(ctx, r.goCommand, goTestArgs(pkg, r.count, r.seed)...)
	stdoutPipe, err := command.StdoutPipe()
	if err != nil {
		_ = eventFile.Close()
		_ = stderrFile.Close()
		return finishPackageResult(result, started, r.now(), 0, 0), nil, err
	}
	command.Stderr = io.MultiWriter(r.stderr, stderrFile, &stderrText)

	_, _ = fmt.Fprintf(r.stdout, "=== stress %s (race, count=%d, seed=%d) ===\n", pkg, r.count, r.seed)
	if err := command.Start(); err != nil {
		finished := r.now().UTC()
		failure := syntheticFailure(pkg, r.runID, err.Error(), finished)
		_ = eventFile.Close()
		_ = stderrFile.Close()
		result = finishPackageResult(result, started, finished, 0, 1)
		return result, []testFailure{failure}, err
	}

	collector := newFailureCollector(pkg, r.runID)
	decoder := json.NewDecoder(io.TeeReader(stdoutPipe, eventFile))
	var decodeErr error
	for {
		var event testEvent
		if err := decoder.Decode(&event); err != nil {
			if !errors.Is(err, io.EOF) {
				decodeErr = fmt.Errorf("decode go test event stream: %w", err)
			}
			break
		}
		collector.consume(event)
		if event.Output != "" {
			_, _ = io.WriteString(r.stdout, event.Output)
		}
	}
	waitErr := command.Wait()
	closeErr := errors.Join(eventFile.Close(), stderrFile.Close())

	finished := r.now().UTC()
	failures := collector.failures
	if len(failures) == 0 && (waitErr != nil || decodeErr != nil || closeErr != nil || collector.packageFailed) {
		text := strings.TrimSpace(strings.Join([]string{
			collector.packageFailureText(),
			stderrText.String(),
			errorText(decodeErr),
			errorText(closeErr),
			errorText(waitErr),
		}, "\n"))
		failures = append(failures, syntheticFailure(pkg, r.runID, text, finished))
	}
	if waitErr == nil && decodeErr == nil && closeErr == nil && !collector.packageFailed && len(failures) == 0 {
		result.Status = "pass"
	}
	result = finishPackageResult(result, started, finished, collector.packageElapsed, len(failures))
	_, _ = fmt.Fprintf(r.stdout, "--- stress %s: %s (%.3fs) ---\n", pkg, result.Status, result.WallDurationSecs)

	var operationalErr error
	if decodeErr != nil || closeErr != nil {
		operationalErr = errors.Join(decodeErr, closeErr)
	}
	if ctx.Err() != nil {
		operationalErr = errors.Join(operationalErr, ctx.Err())
	} else if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			operationalErr = errors.Join(operationalErr, waitErr)
		}
	}
	return result, failures, operationalErr
}

func goTestArgs(pkg string, count int, seed int64) []string {
	return []string{
		"test",
		"-json",
		"-race",
		"-count=" + strconv.Itoa(count),
		"-shuffle=" + strconv.FormatInt(seed, 10),
		pkg,
	}
}

func finishPackageResult(
	result packageResult,
	started time.Time,
	finished time.Time,
	testElapsed float64,
	failures int,
) packageResult {
	result.FinishedAt = finished.UTC()
	result.WallDurationSecs = finished.Sub(started).Seconds()
	result.TestElapsedSecs = testElapsed
	result.StructuredFailures = failures
	return result
}

type failureCollector struct {
	pkg            string
	runID          string
	testOutput     map[string][]string
	packageOutput  []string
	failures       []testFailure
	failureIndex   map[string]int
	packageElapsed float64
	packageFailed  bool
}

func newFailureCollector(pkg, runID string) *failureCollector {
	return &failureCollector{
		pkg:          pkg,
		runID:        runID,
		testOutput:   make(map[string][]string),
		failures:     make([]testFailure, 0),
		failureIndex: make(map[string]int),
	}
}

func (c *failureCollector) consume(event testEvent) {
	switch {
	case event.Action == "run" && event.Test != "":
		c.testOutput[event.Test] = nil
	case event.Action == "output" && event.Test != "":
		c.testOutput[event.Test] = append(c.testOutput[event.Test], event.Output)
	case event.Action == "output":
		c.packageOutput = append(c.packageOutput, event.Output)
	case event.Action == "fail" && event.Test != "":
		c.add(event.Test, strings.Join(c.testOutput[event.Test], ""), event.Time)
		delete(c.testOutput, event.Test)
	case event.Action == "fail":
		c.packageFailed = true
		c.packageElapsed = event.Elapsed
	case event.Action == "pass" && event.Test == "":
		c.packageElapsed = event.Elapsed
	}
}

func (c *failureCollector) add(test, text string, observed time.Time) {
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "test reported failure without output"
	}
	text, truncated := truncateFailureText(text)
	fingerprint := failureFingerprint(c.pkg, test)
	if index, ok := c.failureIndex[fingerprint]; ok {
		c.failures[index].LastSeenAt = observed
		c.failures[index].LastSeenRun = c.runID
		c.failures[index].Occurrences++
		return
	}
	c.failureIndex[fingerprint] = len(c.failures)
	c.failures = append(c.failures, testFailure{
		Fingerprint:          fingerprint,
		Package:              c.pkg,
		Test:                 test,
		FailureText:          text,
		FailureTextTruncated: truncated,
		FirstSeenRun:         c.runID,
		LastSeenRun:          c.runID,
		FirstSeenAt:          observed,
		LastSeenAt:           observed,
		Occurrences:          1,
	})
}

func (c *failureCollector) packageFailureText() string {
	return strings.TrimSpace(strings.Join(c.packageOutput, ""))
}

func syntheticFailure(pkg, runID, text string, observed time.Time) testFailure {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "package failed without output"
	}
	text, truncated := truncateFailureText(text)
	return testFailure{
		Fingerprint:          failureFingerprint(pkg, "(package)"),
		Package:              pkg,
		Test:                 "(package)",
		FailureText:          text,
		FailureTextTruncated: truncated,
		FirstSeenRun:         runID,
		LastSeenRun:          runID,
		FirstSeenAt:          observed,
		LastSeenAt:           observed,
		Occurrences:          1,
	}
}

func truncateFailureText(text string) (string, bool) {
	if len(text) <= failureTextLimit {
		return text, false
	}
	return text[:failureTextLimit], true
}

// Test output contains volatile durations, addresses, and goroutine IDs; keep
// the ledger key stable while retaining the complete failure text separately.
func failureFingerprint(pkg, test string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(pkg+"\x00"+test)))
}

func artifactBase(pkg string) string {
	value := strings.TrimPrefix(filepath.ToSlash(pkg), "./")
	var base strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '.', char == '-', char == '_':
			base.WriteRune(char)
		default:
			base.WriteByte('_')
		}
	}
	if base.Len() == 0 {
		return "package"
	}
	return base.String()
}

func writeReports(outputDir string, summary summaryReport, failures failuresReport) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outputDir, "summary.json"), summary); err != nil {
		return err
	}
	return writeJSON(filepath.Join(outputDir, "failures.json"), failures)
}

func writeJSON(path string, value any) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func metadataFromEnvironment(getenv func(string) string, started time.Time) runMetadata {
	runID := getenv("GITHUB_RUN_ID")
	if runID == "" {
		runID = "local-" + started.UTC().Format("20060102T150405.000000000Z")
	}
	attempt := envOrDefault(getenv, "GITHUB_RUN_ATTEMPT", "1")
	repository := getenv("GITHUB_REPOSITORY")
	url := ""
	if server := getenv("GITHUB_SERVER_URL"); server != "" && repository != "" && getenv("GITHUB_RUN_ID") != "" {
		url = strings.TrimSuffix(server, "/") + "/" + repository + "/actions/runs/" + getenv("GITHUB_RUN_ID")
	}
	return runMetadata{
		RunID:      runID,
		RunAttempt: attempt,
		Trigger:    envOrDefault(getenv, "GITHUB_EVENT_NAME", "local"),
		Repository: repository,
		Ref:        getenv("GITHUB_REF"),
		SHA:        getenv("GITHUB_SHA"),
		Workflow:   getenv("GITHUB_WORKFLOW"),
		Job:        getenv("GITHUB_JOB"),
		Actor:      getenv("GITHUB_ACTOR"),
		URL:        url,
		RunnerOS:   runtime.GOOS,
		RunnerArch: runtime.GOARCH,
		GoVersion:  runtime.Version(),
		StartedAt:  started,
	}
}

func envOrDefault(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
