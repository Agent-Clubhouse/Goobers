package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	results []commandResult
	calls   []fakeCall
	cancel  context.CancelFunc
}

type fakeCall struct {
	args  []string
	cache string
}

func (runner *fakeRunner) Run(_ context.Context, _ string, args []string, _ string, env []string) commandResult {
	call := fakeCall{args: slices.Clone(args)}
	for _, entry := range env {
		if len(entry) > len("GOCACHE=") && entry[:len("GOCACHE=")] == "GOCACHE=" {
			call.cache = entry[len("GOCACHE="):]
		}
	}
	runner.calls = append(runner.calls, call)
	if runner.cancel != nil {
		runner.cancel()
		runner.cancel = nil
	}
	if len(runner.results) == 0 {
		return commandResult{DurationNS: int64(len(runner.calls))}
	}
	result := runner.results[0]
	runner.results = runner.results[1:]
	return result
}

func testOptions() options {
	return options{
		checkout: "checkout", revision: "abc", packages: stringList{"./internal/a", "./internal/b"},
		tests: stringList{"TestOne", "TestTwo"}, repeats: 2, build: "./cmd/goobers",
	}
}

func testSource() sourceInfo {
	return sourceInfo{Checkout: "checkout", Revision: "abc"}
}

func testBuild() buildContext {
	return buildContext{GoVersion: "go1.26", Compiler: "gc", GOOS: "windows", GOARCH: "amd64", CGO: "0"}
}

func TestWarmWorkloadsArePrimedAndRepeatIdenticalCommands(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{}
	rep := benchmark(context.Background(), testOptions(), testSource(), testBuild(), root, runner)

	if rep.FinalStatus != 0 || len(rep.Workloads) != 12 {
		t.Fatalf("status/workloads = %d/%d, want 0/12", rep.FinalStatus, len(rep.Workloads))
	}
	call := 0
	for _, workload := range rep.Workloads {
		wantCalls := 2
		if workload.CacheState == "warm" {
			wantCalls++
			if workload.Prime == nil {
				t.Errorf("%s has no prime result", workload.Name)
			}
		}
		group := runner.calls[call : call+wantCalls]
		for _, measured := range group[1:] {
			if !reflect.DeepEqual(measured.args, group[0].args) {
				t.Errorf("%s command changed between prime/sample: %q != %q", workload.Name, measured.args, group[0].args)
			}
		}
		if workload.CacheState == "warm" {
			for _, measured := range group[1:] {
				if measured.cache != group[0].cache {
					t.Errorf("%s cache changed between prime/sample: %q != %q", workload.Name, measured.cache, group[0].cache)
				}
			}
		}
		call += wantCalls
	}
}

func TestColdSamplesUseNewIsolatedCaches(t *testing.T) {
	opts := testOptions()
	opts.repeats = 3
	runner := &fakeRunner{}
	rep := benchmark(context.Background(), opts, testSource(), testBuild(), t.TempDir(), runner)

	call := 0
	for _, workload := range rep.Workloads {
		count := opts.repeats
		if workload.CacheState == "warm" {
			count++
		}
		group := runner.calls[call : call+count]
		if workload.CacheState == "cold" {
			seen := map[string]bool{}
			for _, measured := range group {
				if seen[measured.cache] {
					t.Errorf("%s reused cold cache %q", workload.Name, measured.cache)
				}
				seen[measured.cache] = true
			}
		}
		call += count
	}
}

func TestFailedSamplesAreRetainedAndSetFinalStatus(t *testing.T) {
	runner := &fakeRunner{results: []commandResult{
		{DurationNS: 10, ExitStatus: 7, Error: "exit status 7"},
		{DurationNS: 20},
	}}
	rep := benchmark(context.Background(), testOptions(), testSource(), testBuild(), t.TempDir(), runner)

	if rep.FinalStatus != 1 {
		t.Fatalf("final status = %d, want 1", rep.FinalStatus)
	}
	first := rep.Workloads[0]
	if len(first.Samples) != 2 || first.Samples[0].ExitStatus != 7 ||
		first.SampleCount != 2 || first.SuccessfulCount != 1 || first.FailedCount != 1 ||
		first.Statistics.Count != 1 || first.Statistics.Minimum != 20 {
		t.Fatalf("failed sample was omitted: %+v", first)
	}
	if len(rep.Workloads) != 12 {
		t.Fatalf("benchmark stopped after command failure: got %d workloads", len(rep.Workloads))
	}
}

func TestCancellationStopsSchedulingAndReturnsPartialReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &fakeRunner{cancel: cancel}
	rep := benchmark(ctx, testOptions(), testSource(), testBuild(), t.TempDir(), runner)

	if !rep.Canceled || rep.FinalStatus != 1 {
		t.Fatalf("canceled/status = %v/%d, want true/1", rep.Canceled, rep.FinalStatus)
	}
	if len(runner.calls) != 1 || len(rep.Workloads) != 1 || len(rep.Workloads[0].Samples) != 1 {
		t.Fatalf("cancellation scheduled extra work: calls/workloads/samples = %d/%d/%d", len(runner.calls), len(rep.Workloads), len(rep.Workloads[0].Samples))
	}
}

func TestSummarize(t *testing.T) {
	got := summarize([]commandResult{
		{DurationNS: 50}, {DurationNS: 10}, {DurationNS: 30}, {DurationNS: 20},
	})
	want := statistics{Count: 4, Minimum: 10, Median: 25, Mean: 27.5, P95: 50, Maximum: 50}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

func TestRunRejectsInvalidRepeatsBeforeInspectingCheckout(t *testing.T) {
	status := run(context.Background(), []string{
		"-checkout", "missing", "-revision", "abc", "-package", "./...", "-test", "Test",
		"-repeats", "0", "-build", "./cmd/goobers", "-out", "out.json", "-cache-root", "cache",
	}, os.Stdout, os.Stderr)
	if status != 2 {
		t.Fatalf("status = %d, want 2", status)
	}
}

func TestToolOwnedCleanupPreservesNamedCacheRoot(t *testing.T) {
	root := t.TempDir()
	cacheRoot := filepath.Join(root, "private-cache")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(cacheRoot, "keep.txt")
	if err := os.WriteFile(named, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	owned, cleanup, err := createOwnedWorkspace(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "temporary"), []byte("remove"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("tool-owned workspace still exists: %v", err)
	}
	if got, err := os.ReadFile(named); err != nil || string(got) != "keep" {
		t.Fatalf("named cache-root content was not preserved: %q, %v", got, err)
	}
}

func TestProcessRunnerUsesInjectedClock(t *testing.T) {
	times := []time.Time{time.Unix(0, 10), time.Unix(0, 35)}
	runner := processRunner{now: func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	}}
	result := runner.Run(context.Background(), "go", []string{"env", "GOVERSION"}, ".", os.Environ())
	if result.ExitStatus != 0 || result.DurationNS != 25 {
		t.Fatalf("result = %+v, want successful 25ns duration", result)
	}
}

func TestSyncedPathDetection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "OneDrive")
	if !syncedPath(filepath.Join(root, "cache"), []string{"OneDrive=" + root}) {
		t.Fatal("OneDrive child was accepted")
	}
	if syncedPath(filepath.Join(t.TempDir(), "cache"), []string{"OneDrive=" + root}) {
		t.Fatal("unrelated path was rejected")
	}
}

func TestWorkloadArgumentsEncodeRequiredModes(t *testing.T) {
	opts := testOptions()
	opts.tags = "integration"
	definitions := workloads(opts, t.TempDir())
	for _, definition := range definitions {
		joined := strings.Join(definition.command.Argv, " ")
		if strings.Contains(definition.name, "race") && !strings.Contains(joined, "-race") {
			t.Errorf("%s omits -race: %q", definition.name, joined)
		}
		if strings.Contains(definition.name, "tests") && !strings.Contains(joined, "-count=1") {
			t.Errorf("%s omits -count=1: %q", definition.name, joined)
		}
		if strings.HasPrefix(definition.name, "no-selected-tests") && !strings.Contains(joined, "-run ^$") {
			t.Errorf("%s does not select zero tests: %q", definition.name, joined)
		}
		if strings.HasPrefix(definition.name, "full-binary-build") && definition.command.Argv[0] != "build" {
			t.Errorf("%s has flags before the go subcommand: %q", definition.name, definition.command.Argv)
		}
	}
}

func TestInspectSourceRequiresPinnedRevisionAndRecordsDirtyState(t *testing.T) {
	ctx := context.Background()
	head, err := commandOutput(ctx, ".", "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	source, err := inspectSource(ctx, ".", head)
	if err != nil {
		t.Fatal(err)
	}
	status, err := commandOutput(ctx, ".", "git", "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		t.Fatal(err)
	}
	if source.Revision != head || source.Dirty != (status != "") {
		t.Fatalf("source = %+v, want revision %s and dirty %v", source, head, status != "")
	}
	if _, err := inspectSource(ctx, ".", strings.Repeat("0", 40)); err == nil {
		t.Fatal("inspectSource accepted a revision other than HEAD")
	}
}

func TestInspectBuildContextRecordsOverrides(t *testing.T) {
	build, err := inspectBuildContext(context.Background(), ".", "first, second", "0")
	if err != nil {
		t.Fatal(err)
	}
	if build.GoVersion == "" || build.Compiler == "" || build.GOOS == "" || build.GOARCH == "" {
		t.Fatalf("build context is incomplete: %+v", build)
	}
	if !reflect.DeepEqual(build.Tags, []string{"first", "second"}) || build.CGO != "0" {
		t.Fatalf("build overrides = tags %q, cgo %q", build.Tags, build.CGO)
	}
}

func TestWriteReportUsesStableFieldOrderAndOmitsCheckout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "report.json")
	rep := report{
		Schema: schemaID, Source: sourceInfo{Checkout: `C:\Users\person\repo`, Revision: strings.Repeat("a", 40)},
		Build: buildContext{Tags: []string{}}, Packages: []string{}, Tests: []string{}, Workloads: []workloadResult{},
	}
	if err := writeReport(path, rep); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("person")) || bytes.Contains(raw, []byte("checkout")) {
		t.Fatalf("report leaked checkout path: %s", raw)
	}
	var decoded report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if !bytes.HasPrefix(raw, []byte("{\n  \"schema\"")) {
		t.Fatalf("report field order changed: %s", raw)
	}
}
