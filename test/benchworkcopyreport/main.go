// Command benchworkcopyreport adds CI runner metadata and trend data to a
// benchworkcopy result and writes a reporting-only workflow summary.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	schemaVersion = 1
	jobName       = "large-repo-provisioning"
)

type benchmarkResult struct {
	Schema              string         `json:"schema"`
	ElapsedMs           int64          `json:"elapsedMs"`
	GOOS                string         `json:"goos"`
	GOARCH              string         `json:"goarch"`
	ColdCloneMs         int64          `json:"coldCloneMs"`
	WarmFetchMs         int64          `json:"warmFetchMs"`
	WorktreeAddMsMedian int64          `json:"worktreeAddMsMedian"`
	TeardownMsMedian    int64          `json:"teardownMsMedian"`
	InitToFirstRunMs    int64          `json:"initToFirstRunMs"`
	SecondRunMs         int64          `json:"secondRunMs"`
	Fixture             *fixtureResult `json:"fixture,omitempty"`
}

type fixtureResult struct {
	GenerateMs int64 `json:"generateMs"`
}

type runnerMetadata struct {
	Class       string `json:"class"`
	Name        string `json:"name"`
	Image       string `json:"image"`
	CPUModel    string `json:"cpuModel"`
	LogicalCPUs int    `json:"logicalCPUs"`
	MemoryBytes int64  `json:"memoryBytes"`
}

type phaseTimings struct {
	FixtureGenerationSeconds float64 `json:"fixtureGenerationSeconds,omitempty"`
	ColdCloneSeconds         float64 `json:"coldCloneSeconds,omitempty"`
	WarmFetchSeconds         float64 `json:"warmFetchSeconds,omitempty"`
	WorktreeAddSeconds       float64 `json:"worktreeAddSeconds,omitempty"`
	TeardownSeconds          float64 `json:"teardownSeconds,omitempty"`
	InitToFirstRunSeconds    float64 `json:"initToFirstRunSeconds,omitempty"`
	SecondRunSeconds         float64 `json:"secondRunSeconds,omitempty"`
}

type trendSample struct {
	RunID          string       `json:"runId"`
	Revision       string       `json:"revision"`
	ElapsedSeconds float64      `json:"elapsedSeconds"`
	Phases         phaseTimings `json:"phases"`
}

type artifact struct {
	SchemaVersion  int             `json:"schemaVersion"`
	Job            string          `json:"job"`
	Platform       string          `json:"platform"`
	Architecture   string          `json:"architecture"`
	ElapsedSeconds float64         `json:"elapsedSeconds"`
	RunID          string          `json:"runId"`
	Revision       string          `json:"revision"`
	Runner         runnerMetadata  `json:"runner"`
	Phases         phaseTimings    `json:"phases"`
	Benchmark      json.RawMessage `json:"benchmark"`
	RecentRuns     []trendSample   `json:"recentRuns"`
}

type options struct {
	current, history, output, summary string
	runID, revision                   string
	runner                            runnerMetadata
	limit                             int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("benchworkcopyreport", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts options
	flags.StringVar(&opts.current, "current", "", "current benchworkcopy JSON path")
	flags.StringVar(&opts.history, "history", "", "directory containing prior report artifacts")
	flags.StringVar(&opts.output, "out", "", "output artifact path")
	flags.StringVar(&opts.summary, "summary", os.Getenv("GITHUB_STEP_SUMMARY"), "Markdown summary path")
	flags.StringVar(&opts.runID, "run-id", "", "workflow run identifier")
	flags.StringVar(&opts.revision, "revision", "", "benchmarked source revision")
	flags.StringVar(&opts.runner.Class, "runner-class", "", "pinned runner class")
	flags.StringVar(&opts.runner.Name, "runner-name", "", "runner name")
	flags.StringVar(&opts.runner.Image, "runner-image", "", "runner image")
	flags.StringVar(&opts.runner.CPUModel, "cpu-model", "", "runner CPU model")
	flags.IntVar(&opts.runner.LogicalCPUs, "logical-cpus", 0, "runner logical CPU count")
	flags.Int64Var(&opts.runner.MemoryBytes, "memory-bytes", 0, "runner memory in bytes")
	flags.IntVar(&opts.limit, "limit", 5, "maximum recent runs to compare")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if opts.current == "" || opts.output == "" || opts.runID == "" || opts.revision == "" ||
		opts.runner.Class == "" || opts.runner.Name == "" || opts.runner.Image == "" ||
		opts.runner.CPUModel == "" || opts.runner.LogicalCPUs < 1 || opts.runner.MemoryBytes < 1 ||
		opts.limit < 1 || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "benchworkcopyreport: current, out, run metadata, positive hardware facts, and a positive limit are required")
		return 2
	}

	raw, current, err := readBenchmark(opts.current)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "benchworkcopyreport: %v\n", err)
		return 1
	}
	recent, warnings := readHistory(opts.history, opts.limit)
	for _, warning := range warnings {
		_, _ = fmt.Fprintf(stderr, "benchworkcopyreport: warning: %v\n", warning)
	}
	result := artifact{
		SchemaVersion: schemaVersion, Job: jobName,
		Platform: current.GOOS, Architecture: current.GOARCH,
		ElapsedSeconds: milliseconds(current.ElapsedMs), RunID: opts.runID,
		Revision: opts.revision, Runner: opts.runner, Phases: phases(current),
		Benchmark: raw, RecentRuns: recent,
	}
	if err := writeJSON(opts.output, result); err != nil {
		_, _ = fmt.Fprintf(stderr, "benchworkcopyreport: write artifact: %v\n", err)
		return 1
	}
	summary := formatSummary(result)
	if opts.summary != "" {
		if err := appendFile(opts.summary, summary); err != nil {
			_, _ = fmt.Fprintf(stderr, "benchworkcopyreport: write summary: %v\n", err)
			return 1
		}
	}
	_, _ = io.WriteString(stdout, summary)
	return 0
}

func readBenchmark(path string) (json.RawMessage, benchmarkResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, benchmarkResult{}, fmt.Errorf("read current benchmark: %w", err)
	}
	var result benchmarkResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, result, fmt.Errorf("decode current benchmark: %w", err)
	}
	if result.Schema != "goobers.bench-workcopy/v2" || result.ElapsedMs < 1 || result.GOOS == "" || result.GOARCH == "" {
		return nil, result, errors.New("current benchmark is missing schema, elapsedMs, or platform metadata")
	}
	return raw, result, nil
}

func readHistory(root string, limit int) ([]trendSample, []error) {
	if root == "" {
		return []trendSample{}, nil
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return []trendSample{}, nil
	} else if err != nil {
		return []trendSample{}, []error{fmt.Errorf("ignore history: inspect %s: %w", root, err)}
	}
	var paths []string
	var warnings []error
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			warnings = append(warnings, fmt.Errorf("ignore history path %s: %w", path, err))
			return nil
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		warnings = append(warnings, fmt.Errorf("ignore history: walk %s: %w", root, err))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	var samples []trendSample
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("ignore history %s: read: %w", path, err))
			continue
		}
		var prior artifact
		if err := json.Unmarshal(raw, &prior); err != nil {
			warnings = append(warnings, fmt.Errorf("ignore history %s: decode: %w", path, err))
			continue
		}
		if prior.SchemaVersion != schemaVersion || prior.Job != jobName || prior.RunID == "" || prior.ElapsedSeconds <= 0 {
			warnings = append(warnings, fmt.Errorf("ignore history %s: not a valid %s artifact", path, jobName))
			continue
		}
		samples = append(samples, trendSample{
			RunID: prior.RunID, Revision: prior.Revision,
			ElapsedSeconds: prior.ElapsedSeconds, Phases: prior.Phases,
		})
		if len(samples) == limit {
			break
		}
	}
	if samples == nil {
		samples = []trendSample{}
	}
	return samples, warnings
}

func phases(result benchmarkResult) phaseTimings {
	var generateMs int64
	if result.Fixture != nil {
		generateMs = result.Fixture.GenerateMs
	}
	return phaseTimings{
		FixtureGenerationSeconds: milliseconds(generateMs),
		ColdCloneSeconds:         milliseconds(result.ColdCloneMs),
		WarmFetchSeconds:         milliseconds(result.WarmFetchMs),
		WorktreeAddSeconds:       milliseconds(result.WorktreeAddMsMedian),
		TeardownSeconds:          milliseconds(result.TeardownMsMedian),
		InitToFirstRunSeconds:    milliseconds(result.InitToFirstRunMs),
		SecondRunSeconds:         milliseconds(result.SecondRunMs),
	}
}

func milliseconds(value int64) float64 {
	return float64(value) / 1000
}

func formatSummary(current artifact) string {
	var builder strings.Builder
	builder.WriteString("## Large-repository provisioning benchmark\n\n")
	fmt.Fprintf(&builder, "Pinned runner: `%s` (`%s`, %d logical CPUs, %.1f GiB memory)\n\n",
		current.Runner.Class, current.Runner.CPUModel, current.Runner.LogicalCPUs,
		float64(current.Runner.MemoryBytes)/(1<<30))
	builder.WriteString("| Measurement | Current | Recent average | Change |\n")
	builder.WriteString("|---|---:|---:|---:|\n")
	writeTrendRow(&builder, "Total", current.ElapsedSeconds, sampleValues(current.RecentRuns, func(s trendSample) float64 { return s.ElapsedSeconds }))
	writeTrendRow(&builder, "Fixture generation", current.Phases.FixtureGenerationSeconds, sampleValues(current.RecentRuns, func(s trendSample) float64 { return s.Phases.FixtureGenerationSeconds }))
	writeTrendRow(&builder, "Init to first run", current.Phases.InitToFirstRunSeconds, sampleValues(current.RecentRuns, func(s trendSample) float64 { return s.Phases.InitToFirstRunSeconds }))
	writeTrendRow(&builder, "Second run", current.Phases.SecondRunSeconds, sampleValues(current.RecentRuns, func(s trendSample) float64 { return s.Phases.SecondRunSeconds }))
	writeTrendRow(&builder, "Cold clone", current.Phases.ColdCloneSeconds, sampleValues(current.RecentRuns, func(s trendSample) float64 { return s.Phases.ColdCloneSeconds }))
	writeTrendRow(&builder, "Warm fetch", current.Phases.WarmFetchSeconds, sampleValues(current.RecentRuns, func(s trendSample) float64 { return s.Phases.WarmFetchSeconds }))
	writeTrendRow(&builder, "Worktree add median", current.Phases.WorktreeAddSeconds, sampleValues(current.RecentRuns, func(s trendSample) float64 { return s.Phases.WorktreeAddSeconds }))
	writeTrendRow(&builder, "Teardown median", current.Phases.TeardownSeconds, sampleValues(current.RecentRuns, func(s trendSample) float64 { return s.Phases.TeardownSeconds }))
	fmt.Fprintf(&builder, "\nComparison uses %d recent successful run(s). This workflow is reporting-only; no timing threshold is enforced.\n", len(current.RecentRuns))
	return builder.String()
}

func sampleValues(samples []trendSample, value func(trendSample) float64) []float64 {
	values := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if item := value(sample); item > 0 {
			values = append(values, item)
		}
	}
	return values
}

func writeTrendRow(builder *strings.Builder, name string, current float64, history []float64) {
	if current <= 0 {
		return
	}
	if len(history) == 0 {
		fmt.Fprintf(builder, "| %s | %.1fs | n/a | n/a |\n", name, current)
		return
	}
	var total float64
	for _, value := range history {
		total += value
	}
	average := total / float64(len(history))
	change := current - average
	percent := 0.0
	if average > 0 {
		percent = change / average * 100
	}
	fmt.Fprintf(builder, "| %s | %.1fs | %.1fs | %+.1fs (%+.1f%%) |\n", name, current, average, change, percent)
}

func writeJSON(path string, value any) error {
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

func appendFile(path, content string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
