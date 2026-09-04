// Command stress repeatedly runs explicitly enrolled packages under the race detector.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/flake"
)

const (
	stressCount = 20
	// stressTimeout is the per-package `go test -timeout` budget. Go's 10m
	// default is a whole-binary budget that ./cmd/goobers exceeds under -race
	// even for a single pass (the Makefile already raises the same workload to
	// GO_TEST_TIMEOUT=30m); at count=20 the default reliably expires mid-run
	// and reports a "panic: test timed out" flake against whichever test
	// happened to be executing (#3167). Keep it generous but finite so a
	// genuine wedge still panics with a goroutine dump instead of burning the
	// whole stress job.
	stressTimeout = 30 * time.Minute
	// stressBuildReserve is the slice of stressTimeout that is not available
	// for repeat execution: compiling and linking a race-instrumented test
	// binary, plus the enumeration pass that resolves a shard's test set,
	// happen inside the same `go test -timeout` window.
	stressBuildReserve    = 10 * time.Minute
	maxShards             = 64
	reportSchema          = "goobers.dev/stress/v1"
	failureTextLimit      = 64 * 1024
	failureSignatureLimit = 1024
)

var testNamePattern = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)

type options struct {
	goCommand   string
	packageList string
	outputDir   string
	seed        int64
	only        string
	list        bool
}

type packageSpec struct {
	Package string
	Count   int
	// Shards splits the package's tests across that many independently
	// scheduled `-run` subsets, each of which repeats Count times.
	Shards int
	// PassBudget is the reviewed wall-clock cost of one race-instrumented
	// pass over the whole package. It is what makes the enrollment budget
	// checkable: Count repetitions of one shard must fit stressTimeout.
	PassBudget time.Duration
}

// shardSpec is one `go test` invocation: a package, optionally narrowed to a
// deterministic slice of its top-level tests.
type shardSpec struct {
	ID      string
	Package string
	Count   int
	Index   int
	Shards  int
}

// shardBudget is the reviewed wall-clock cost of running one shard of spec,
// including the build reserve the invocation cannot avoid paying.
func (s packageSpec) shardBudget() time.Duration {
	return time.Duration(s.Count)*s.PassBudget/time.Duration(s.Shards) + stressBuildReserve
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
	Shard              string    `json:"shard"`
	ShardIndex         int       `json:"shard_index"`
	Shards             int       `json:"shards"`
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
	FailureSignature     string    `json:"failure_signature"`
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
	shards := expandShards(packages)
	if opts.list {
		encoded, err := json.Marshal(shardIDs(shards))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "stress: encode shard list: %v\n", err)
			return 2
		}
		_, _ = fmt.Fprintf(stdout, "%s\n", encoded)
		return 0
	}
	if opts.only != "" {
		selected, err := selectShard(shards, opts.only)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "stress: %v\n", err)
			return 2
		}
		shards = []shardSpec{selected}
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

	summary, failures, executeErr := executeStress(context.Background(), runner, shards)
	metadata.FinishedAt = now().UTC()
	summary.Run = metadata
	failures.Run = metadata
	if err := writeReports(opts.outputDir, summary, failures); err != nil {
		_, _ = fmt.Fprintf(stderr, "stress: write reports: %v\n", err)
		return 2
	}

	_, _ = fmt.Fprintf(stdout, "stress: %s (%d shard(s)); reports: %s\n",
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
	flags.StringVar(&opts.only, "only", "", "run a single shard by id (see -list)")
	flags.BoolVar(&opts.list, "list", false, "print the enrolled shard ids as a JSON array and exit")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/stress [-go binary] [-packages file] [-output dir] [-seed n] [-list] [-only shard]")
		return options{}, errors.New("unexpected positional arguments")
	}
	if strings.TrimSpace(opts.goCommand) == "" || strings.TrimSpace(opts.packageList) == "" ||
		strings.TrimSpace(opts.outputDir) == "" || opts.seed < 0 {
		_, _ = fmt.Fprintln(stderr, "stress: -go, -packages, and -output must be non-empty; -seed must be non-negative")
		return options{}, errors.New("invalid options")
	}
	return opts, nil
}

func loadPackages(r io.Reader) ([]packageSpec, error) {
	scanner := bufio.NewScanner(r)
	seen := make(map[string]struct{})
	var packages []packageSpec
	for line := 1; scanner.Scan(); line++ {
		fields := strings.Fields(strings.SplitN(scanner.Text(), "#", 2)[0])
		if len(fields) == 0 {
			continue
		}
		pkg := fields[0]
		if !strings.HasPrefix(pkg, "./") {
			return nil, fmt.Errorf("line %d: package must be a relative Go package pattern, got %q", line, pkg)
		}
		spec, err := parseSettings(pkg, fields[1:])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if _, exists := seen[pkg]; exists {
			return nil, fmt.Errorf("line %d: duplicate package %q", line, pkg)
		}
		seen[pkg] = struct{}{}
		packages = append(packages, spec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(packages) == 0 {
		return nil, errors.New("package list is empty")
	}
	return packages, nil
}

// parseSettings reads the `key=value` enrollment settings that follow a package
// pattern. Every entry must declare `pass=`, the reviewed cost of one race pass,
// so the budget its repetitions imply is checkable here rather than discovered
// as a nightly timeout (#4222).
func parseSettings(pkg string, fields []string) (packageSpec, error) {
	spec := packageSpec{Package: pkg, Count: stressCount, Shards: 1}
	declared := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return packageSpec{}, fmt.Errorf("expected key=value settings, got %q", field)
		}
		if _, repeated := declared[key]; repeated {
			return packageSpec{}, fmt.Errorf("duplicate setting %q", key)
		}
		declared[key] = struct{}{}
		switch key {
		case "count":
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > stressCount {
				return packageSpec{}, fmt.Errorf("count must be between 1 and %d, got %q", stressCount, value)
			}
			spec.Count = parsed
		case "shards":
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > maxShards {
				return packageSpec{}, fmt.Errorf("shards must be between 1 and %d, got %q", maxShards, value)
			}
			spec.Shards = parsed
		case "pass":
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				return packageSpec{}, fmt.Errorf("pass must be a positive duration, got %q", value)
			}
			spec.PassBudget = parsed
		default:
			return packageSpec{}, fmt.Errorf("expected count=N, shards=N, or pass=DURATION, got %q", field)
		}
	}
	if spec.PassBudget == 0 {
		return packageSpec{}, errors.New("missing pass=DURATION, the reviewed cost of one race pass")
	}
	if budget := spec.shardBudget(); budget > stressTimeout {
		return packageSpec{}, fmt.Errorf(
			"budget %v (count=%d of %v over %d shard(s) plus %v build reserve) exceeds the %v per-shard timeout; raise shards= or lower count=",
			budget, spec.Count, spec.PassBudget, spec.Shards, stressBuildReserve, stressTimeout)
	}
	return spec, nil
}

// expandShards flattens the enrollment into the individual `go test`
// invocations the nightly matrix schedules, one per shard.
func expandShards(packages []packageSpec) []shardSpec {
	shards := make([]shardSpec, 0, len(packages))
	for _, spec := range packages {
		for index := range spec.Shards {
			shards = append(shards, shardSpec{
				ID:      shardID(spec.Package, index, spec.Shards),
				Package: spec.Package,
				Count:   spec.Count,
				Index:   index,
				Shards:  spec.Shards,
			})
		}
	}
	return shards
}

func shardID(pkg string, index, shards int) string {
	base := artifactBase(pkg)
	if shards <= 1 {
		return base
	}
	return fmt.Sprintf("%s-%02dof%02d", base, index+1, shards)
}

func shardIDs(shards []shardSpec) []string {
	ids := make([]string, 0, len(shards))
	for _, shard := range shards {
		ids = append(ids, shard.ID)
	}
	return ids
}

func selectShard(shards []shardSpec, id string) (shardSpec, error) {
	for _, shard := range shards {
		if shard.ID == id {
			return shard, nil
		}
	}
	return shardSpec{}, fmt.Errorf("unknown shard %q; -list prints the enrolled shard ids", id)
}

func executeStress(ctx context.Context, runner processRunner, shards []shardSpec) (summaryReport, failuresReport, error) {
	summary := summaryReport{
		SchemaVersion: reportSchema,
		Status:        "pass",
		Race:          true,
		Count:         runner.count,
		Seed:          runner.seed,
		FailuresFile:  "failures.json",
		Packages:      make([]packageResult, 0, len(shards)),
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
	for _, spec := range shards {
		packageRunner := runner
		packageRunner.count = spec.Count
		result, packageFailures, err := packageRunner.runShard(ctx, spec)
		summary.Packages = append(summary.Packages, result)
		failures.Failures = append(failures.Failures, packageFailures...)
		if result.Status != "pass" {
			summary.Status = "fail"
		}
		if err != nil {
			executionErr = errors.Join(executionErr, fmt.Errorf("%s: %w", spec.ID, err))
		}
	}
	return summary, failures, executionErr
}

func (r processRunner) runShard(ctx context.Context, spec shardSpec) (packageResult, []testFailure, error) {
	started := r.now().UTC()
	eventRel := filepath.ToSlash(filepath.Join("packages", spec.ID+".jsonl"))
	stderrRel := filepath.ToSlash(filepath.Join("packages", spec.ID+".stderr.txt"))
	pkg := spec.Package
	result := packageResult{
		Package:    pkg,
		Shard:      spec.ID,
		ShardIndex: spec.Index,
		Shards:     spec.Shards,
		Status:     "fail",
		Race:       true,
		Count:      r.count,
		Seed:       r.seed,
		StartedAt:  started,
		EventLog:   eventRel,
		StderrLog:  stderrRel,
	}

	if err := os.MkdirAll(filepath.Join(r.outputDir, "packages"), 0o755); err != nil {
		return finishPackageResult(result, started, r.now(), 0, 0), nil, err
	}
	runPattern, err := r.shardRunPattern(ctx, spec)
	if err != nil {
		finished := r.now().UTC()
		failure := syntheticFailure(pkg, r.runID, err.Error(), finished)
		return finishPackageResult(result, started, finished, 0, 1), []testFailure{failure}, err
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
	command := r.command(ctx, r.goCommand, goTestArgs(pkg, r.count, r.seed, runPattern)...)
	stdoutPipe, err := command.StdoutPipe()
	if err != nil {
		_ = eventFile.Close()
		_ = stderrFile.Close()
		return finishPackageResult(result, started, r.now(), 0, 0), nil, err
	}
	command.Stderr = io.MultiWriter(r.stderr, stderrFile, &stderrText)

	_, _ = fmt.Fprintf(r.stdout, "=== stress %s (race, count=%d, seed=%d) ===\n", spec.ID, r.count, r.seed)
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
	_, _ = fmt.Fprintf(r.stdout, "--- stress %s: %s (%.3fs) ---\n", spec.ID, result.Status, result.WallDurationSecs)

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

func goTestArgs(pkg string, count int, seed int64, runPattern string) []string {
	args := []string{
		"test",
		"-json",
		"-race",
		"-count=" + strconv.Itoa(count),
		"-timeout=" + stressTimeout.String(),
		"-shuffle=" + strconv.FormatInt(seed, 10),
	}
	if runPattern != "" {
		args = append(args, "-run="+runPattern)
	}
	return append(args, pkg)
}

func goListArgs(pkg string) []string {
	// Enumerate with the same -race build the repetitions use so the shard
	// pays for the race-instrumented test binary once.
	return []string{"test", "-race", "-list=^Test", pkg}
}

// shardRunPattern enumerates the package's top-level tests and returns a -run
// pattern selecting this shard's deterministic slice of them. Round-robin
// assignment keeps shards balanced as tests are added, so the split does not
// need hand-maintained name prefixes.
func (r processRunner) shardRunPattern(ctx context.Context, spec shardSpec) (string, error) {
	if spec.Shards <= 1 {
		return "", nil
	}
	var listed bytes.Buffer
	command := r.command(ctx, r.goCommand, goListArgs(spec.Package)...)
	command.Stdout = &listed
	command.Stderr = r.stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("list tests in %s: %w", spec.Package, err)
	}
	var selected []string
	index := 0
	for _, line := range strings.Split(listed.String(), "\n") {
		name := strings.TrimSpace(line)
		if !testNamePattern.MatchString(name) {
			continue
		}
		if index%spec.Shards == spec.Index {
			selected = append(selected, name)
		}
		index++
	}
	if len(selected) == 0 {
		return "", fmt.Errorf("shard %s selected no tests from %s (%d test(s) listed)", spec.ID, spec.Package, index)
	}
	return "^(?:" + strings.Join(selected, "|") + ")$", nil
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
	signature := normalizeFailureSignature(text)
	text, truncated := truncateFailureText(text)
	fingerprint := failureFingerprint(c.pkg, test, signature)
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
		FailureSignature:     signature,
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
	signature := normalizeFailureSignature(text)
	text, truncated := truncateFailureText(text)
	return testFailure{
		Fingerprint:          failureFingerprint(pkg, "(package)", signature),
		Package:              pkg,
		Test:                 "(package)",
		FailureSignature:     signature,
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

func normalizeFailureSignature(text string) string {
	return flake.NormalizeSignature(text)
}

func failureFingerprint(pkg, test, signature string) string {
	return flake.Fingerprint(pkg, test, signature)
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
