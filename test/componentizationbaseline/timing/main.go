// Command timing records repeatable extraction-baseline timings with isolated
// Go build caches.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	schemaID       = "goobers.componentization-timing/v1"
	defaultRepeats = 5
)

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("value must not be empty")
	}
	*values = append(*values, value)
	return nil
}

type options struct {
	checkout  string
	revision  string
	packages  stringList
	tests     stringList
	repeats   int
	tags      string
	cgo       string
	build     string
	output    string
	cacheRoot string
}

type sourceInfo struct {
	Checkout string `json:"-"`
	Revision string `json:"revision"`
	Dirty    bool   `json:"dirty"`
}

type buildContext struct {
	GoVersion string   `json:"goVersion"`
	Compiler  string   `json:"compiler"`
	GOOS      string   `json:"goos"`
	GOARCH    string   `json:"goarch"`
	Tags      []string `json:"tags"`
	CGO       string   `json:"cgoEnabled"`
}

type commandSpec struct {
	Executable string   `json:"executable"`
	Argv       []string `json:"argv"`
}

type commandResult struct {
	DurationNS int64  `json:"durationNs"`
	ExitStatus int    `json:"exitStatus"`
	Error      string `json:"error,omitempty"`
}

type statistics struct {
	Count   int     `json:"count"`
	Minimum int64   `json:"minimumNs"`
	Median  int64   `json:"medianNs"`
	Mean    float64 `json:"meanNs"`
	P95     int64   `json:"p95Ns"`
	Maximum int64   `json:"maximumNs"`
}

type workloadResult struct {
	Name            string          `json:"name"`
	Mode            string          `json:"mode"`
	CacheState      string          `json:"cacheState"`
	Command         commandSpec     `json:"command"`
	Prime           *commandResult  `json:"prime,omitempty"`
	SampleCount     int             `json:"sampleCount"`
	SuccessfulCount int             `json:"successfulCount"`
	FailedCount     int             `json:"failedCount"`
	Samples         []commandResult `json:"samples"`
	Statistics      statistics      `json:"statistics"`
}

type report struct {
	Schema      string           `json:"schema"`
	Source      sourceInfo       `json:"source"`
	Build       buildContext     `json:"buildContext"`
	Repeats     int              `json:"repeats"`
	Packages    []string         `json:"packages"`
	Tests       []string         `json:"tests"`
	BuildTarget string           `json:"buildTarget"`
	Workloads   []workloadResult `json:"workloads"`
	Canceled    bool             `json:"canceled"`
	FinalStatus int              `json:"finalStatus"`
}

type runner interface {
	Run(context.Context, string, []string, string, []string) commandResult
}

type processRunner struct {
	now func() time.Time
}

func (r processRunner) Run(ctx context.Context, executable string, args []string, dir string, env []string) commandResult {
	started := r.now()
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = dir
	command.Env = env
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	err := command.Run()
	result := commandResult{DurationNS: r.now().Sub(started).Nanoseconds()}
	if err == nil {
		return result
	}
	result.ExitStatus = 1
	result.Error = err.Error()
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitStatus = exitError.ExitCode()
	}
	return result
}

type workload struct {
	name, mode, cacheState string
	command                commandSpec
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("componentization-timing", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts options
	flags.StringVar(&opts.checkout, "checkout", "", "path to the pinned source checkout")
	flags.StringVar(&opts.revision, "revision", "", "required full Git revision at checkout HEAD")
	flags.Var(&opts.packages, "package", "package selector to test (repeatable)")
	flags.Var(&opts.tests, "test", "test name regular expression (repeatable)")
	flags.IntVar(&opts.repeats, "repeats", defaultRepeats, "sample count per workload")
	flags.StringVar(&opts.tags, "tags", "", "comma-separated Go build tags")
	flags.StringVar(&opts.cgo, "cgo", "", "CGO_ENABLED value (0 or 1; default from go env)")
	flags.StringVar(&opts.build, "build", "./cmd/goobers", "full-binary build target")
	flags.StringVar(&opts.output, "out", "", "JSON evidence output path")
	flags.StringVar(&opts.cacheRoot, "cache-root", "", "private, non-synced root for tool-owned build caches")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || opts.checkout == "" || opts.revision == "" || len(opts.packages) == 0 ||
		len(opts.tests) == 0 || opts.output == "" || opts.cacheRoot == "" || opts.build == "" {
		_, _ = fmt.Fprintln(stderr, "componentization-timing: checkout, revision, package, test, build, out, and cache-root are required; positional arguments are not accepted")
		return 2
	}
	if opts.repeats < 1 {
		_, _ = fmt.Fprintln(stderr, "componentization-timing: repeats must be at least 1")
		return 2
	}
	if opts.cgo != "" && opts.cgo != "0" && opts.cgo != "1" {
		_, _ = fmt.Fprintln(stderr, "componentization-timing: cgo must be 0 or 1")
		return 2
	}

	checkout, err := filepath.Abs(opts.checkout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "componentization-timing: resolve checkout: %v\n", err)
		return 1
	}
	cacheRoot, err := filepath.Abs(opts.cacheRoot)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "componentization-timing: resolve cache root: %v\n", err)
		return 1
	}
	if syncedPath(cacheRoot, os.Environ()) {
		_, _ = fmt.Fprintln(stderr, "componentization-timing: cache-root must not be inside OneDrive or another configured synced root")
		return 2
	}
	source, err := inspectSource(ctx, checkout, opts.revision)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "componentization-timing: %v\n", err)
		return 1
	}
	build, err := inspectBuildContext(ctx, checkout, opts.tags, opts.cgo)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "componentization-timing: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "componentization-timing: create cache root: %v\n", err)
		return 1
	}
	ownedRoot, cleanup, err := createOwnedWorkspace(cacheRoot)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "componentization-timing: create private cache workspace: %v\n", err)
		return 1
	}

	rep := benchmark(ctx, opts, source, build, ownedRoot, processRunner{now: time.Now})
	if err := writeReport(opts.output, rep); err != nil {
		_ = cleanup()
		_, _ = fmt.Fprintf(stderr, "componentization-timing: write evidence: %v\n", err)
		return 1
	}
	if err := cleanup(); err != nil {
		_, _ = fmt.Fprintf(stderr, "componentization-timing: clean private cache workspace: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "componentization-timing: wrote %d workloads to %s (status %d)\n", len(rep.Workloads), opts.output, rep.FinalStatus)
	return rep.FinalStatus
}

func createOwnedWorkspace(cacheRoot string) (string, func() error, error) {
	path, err := os.MkdirTemp(cacheRoot, "componentization-timing-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error { return os.RemoveAll(path) }
	for _, child := range []string{"caches", "outputs"} {
		if err := os.Mkdir(filepath.Join(path, child), 0o755); err != nil {
			_ = cleanup()
			return "", nil, err
		}
	}
	return path, cleanup, nil
}

func benchmark(ctx context.Context, opts options, source sourceInfo, build buildContext, ownedRoot string, commandRunner runner) report {
	rep := report{
		Schema: schemaID, Source: source, Build: build, Repeats: opts.repeats,
		Packages: slices.Clone(opts.packages), Tests: slices.Clone(opts.tests),
		BuildTarget: opts.build, Workloads: []workloadResult{},
	}
	baseEnv := withEnv(os.Environ(), "CGO_ENABLED", build.CGO)
	for _, definition := range workloads(opts, ownedRoot) {
		if ctx.Err() != nil {
			rep.Canceled = true
			break
		}
		result := workloadResult{
			Name: definition.name, Mode: definition.mode, CacheState: definition.cacheState,
			Command: definition.command, Samples: []commandResult{},
		}
		cache := filepath.Join(ownedRoot, "caches", definition.name)
		if definition.cacheState == "warm" {
			prime := commandResult{}
			if err := os.MkdirAll(cache, 0o755); err != nil {
				prime = commandResult{ExitStatus: 1, Error: "create warm cache: " + err.Error()}
			} else {
				prime = commandRunner.Run(ctx, definition.command.Executable, definition.command.Argv, source.Checkout, withEnv(baseEnv, "GOCACHE", cache))
			}
			result.Prime = &prime
			if prime.ExitStatus != 0 {
				rep.FinalStatus = 1
			}
		}
		for sample := 0; sample < opts.repeats && ctx.Err() == nil; sample++ {
			sampleCache := cache
			if definition.cacheState == "cold" {
				sampleCache = filepath.Join(cache, strconv.Itoa(sample))
				if err := os.MkdirAll(sampleCache, 0o755); err != nil {
					result.Samples = append(result.Samples, commandResult{ExitStatus: 1, Error: "create cold cache: " + err.Error()})
					rep.FinalStatus = 1
					continue
				}
			}
			measured := commandRunner.Run(ctx, definition.command.Executable, definition.command.Argv, source.Checkout, withEnv(baseEnv, "GOCACHE", sampleCache))
			result.Samples = append(result.Samples, measured)
			if measured.ExitStatus != 0 {
				rep.FinalStatus = 1
			}
		}
		result.SampleCount = len(result.Samples)
		for _, sample := range result.Samples {
			if sample.ExitStatus == 0 {
				result.SuccessfulCount++
			} else {
				result.FailedCount++
			}
		}
		result.Statistics = summarize(result.Samples)
		rep.Workloads = append(rep.Workloads, result)
	}
	if ctx.Err() != nil {
		rep.Canceled = true
		rep.FinalStatus = 1
	}
	return rep
}

func workloads(opts options, ownedRoot string) []workload {
	selected := testArgs(opts, strings.Join(opts.tests, "|"))
	noSelected := testArgs(opts, "^$")
	binary := filepath.Join(ownedRoot, "outputs", "goobers")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	buildArgs := []string{"build"}
	buildArgs = append(buildArgs, tagArgs(opts.tags)...)
	buildArgs = append(buildArgs, "-o", binary, opts.build)

	var definitions []workload
	for _, scenario := range []struct {
		name string
		args []string
	}{
		{name: "selected-tests", args: selected},
		{name: "no-selected-tests", args: noSelected},
		{name: "full-binary-build", args: buildArgs},
	} {
		for _, mode := range []string{"normal", "race"} {
			args := slices.Clone(scenario.args)
			if mode == "race" {
				args = addRace(args)
			}
			for _, cacheState := range []string{"cold", "warm"} {
				name := scenario.name + "-" + mode + "-" + cacheState
				definitions = append(definitions, workload{
					name: name, mode: mode, cacheState: cacheState,
					command: commandSpec{Executable: "go", Argv: args},
				})
			}
		}
	}
	return definitions
}

func testArgs(opts options, testPattern string) []string {
	args := []string{"test", "-count=1"}
	if opts.tags != "" {
		args = append(args, "-tags", opts.tags)
	}
	args = append(args, opts.packages...)
	return append(args, "-run", testPattern)
}

func tagArgs(tags string) []string {
	if tags == "" {
		return nil
	}
	return []string{"-tags", tags}
}

func addRace(args []string) []string {
	for i, arg := range args {
		if arg == "test" || arg == "build" {
			return slices.Insert(args, i+1, "-race")
		}
	}
	return args
}

func summarize(samples []commandResult) statistics {
	if len(samples) == 0 {
		return statistics{}
	}
	values := make([]int64, len(samples))
	var total int64
	values = values[:0]
	for _, sample := range samples {
		if sample.ExitStatus != 0 {
			continue
		}
		values = append(values, sample.DurationNS)
		total += sample.DurationNS
	}
	if len(values) == 0 {
		return statistics{}
	}
	slices.Sort(values)
	middle := len(values) / 2
	median := values[middle]
	if len(values)%2 == 0 {
		median = values[middle-1] + (values[middle]-values[middle-1])/2
	}
	p95 := values[int(math.Ceil(float64(len(values))*0.95))-1]
	return statistics{
		Count: len(values), Minimum: values[0], Median: median,
		Mean: float64(total) / float64(len(values)), P95: p95, Maximum: values[len(values)-1],
	}
}

func inspectSource(ctx context.Context, checkout, revision string) (sourceInfo, error) {
	head, err := commandOutput(ctx, checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return sourceInfo{}, fmt.Errorf("inspect source revision: %w", err)
	}
	if head != revision {
		return sourceInfo{}, fmt.Errorf("checkout HEAD %s does not match pinned revision %s", head, revision)
	}
	status, err := commandOutput(ctx, checkout, "git", "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return sourceInfo{}, fmt.Errorf("inspect source dirty state: %w", err)
	}
	return sourceInfo{Checkout: checkout, Revision: head, Dirty: status != ""}, nil
}

func inspectBuildContext(ctx context.Context, checkout, tags, cgoOverride string) (buildContext, error) {
	output, err := commandOutput(ctx, checkout, "go", "env", "GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED")
	if err != nil {
		return buildContext{}, fmt.Errorf("inspect Go build context: %w", err)
	}
	lines := strings.Split(output, "\n")
	if len(lines) != 4 {
		return buildContext{}, fmt.Errorf("inspect Go build context: expected four go env values, got %d", len(lines))
	}
	cgo := lines[3]
	if cgoOverride != "" {
		cgo = cgoOverride
	}
	var buildTags []string
	for _, tag := range strings.Split(tags, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			buildTags = append(buildTags, tag)
		}
	}
	return buildContext{
		GoVersion: lines[0], Compiler: runtime.Compiler, GOOS: lines[1], GOARCH: lines[2],
		Tags: buildTags, CGO: cgo,
	}, nil
}

func commandOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func withEnv(env []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(strings.ToUpper(entry), prefix) {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func syncedPath(path string, env []string) bool {
	path = filepath.Clean(path)
	for _, name := range []string{"OneDrive", "OneDriveCommercial", "OneDriveConsumer", "DROPBOX", "GOOGLE_DRIVE"} {
		for _, entry := range env {
			key, value, ok := strings.Cut(entry, "=")
			if ok && strings.EqualFold(key, name) && value != "" && pathWithin(path, value) {
				return true
			}
		}
	}
	return false
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func writeReport(path string, value report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
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
