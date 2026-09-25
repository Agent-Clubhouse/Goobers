package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validSplitInputs() ([]artifact, runMetadata) {
	timing := artifact{
		SchemaVersion: schemaVersion, Job: "unit", Platform: "linux", Architecture: "amd64", ElapsedSeconds: 10,
		Tests: []testTiming{
			{Package: "example/big", Test: "TestA", Status: "pass", ElapsedSeconds: 1.234},
			{Package: "example/big", Test: "TestA/sub", Status: "pass", ElapsedSeconds: 1},
			{Package: "example/big", Test: "TestSkipped", Status: "skip", ElapsedSeconds: 0},
			{Package: "example/other", Test: "TestOther", Status: "fail", ElapsedSeconds: 3},
		},
	}
	run := runMetadata{
		ID: 42, HeadBranch: "main", HeadSHA: strings.Repeat("c", 40),
		Status: "completed", Conclusion: "success", UpdatedAt: "2026-09-25T00:19:09Z",
	}
	return []artifact{timing}, run
}

func TestGenerateSplitsRecordsTopLevelTestsAndProvenance(t *testing.T) {
	timings, run := validSplitInputs()
	got, err := generateSplits(timings, run, map[string]int{"example/big": 3})
	if err != nil {
		t.Fatal(err)
	}
	want := splitDocument{
		SchemaVersion: schemaVersion,
		Source: splitSource{
			Run: 42, Commit: strings.Repeat("c", 40), GeneratedAt: "2026-09-25T00:19:09Z",
			TimingJobs: []string{"unit"}, Platform: "linux",
		},
		Packages: map[string]splitPackage{
			"example/big": {Pieces: 3, Tests: map[string]float64{"TestA": 1.23, "TestSkipped": 0}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splits = %+v, want %+v", got, want)
	}
}

func TestGenerateSplitsMergesRaceShardParts(t *testing.T) {
	timings, run := validSplitInputs()
	part := timings[0]
	part.Job = "unit-shard"
	part.Tests = []testTiming{{Package: "example/big", Test: "TestB", Status: "pass", ElapsedSeconds: 2}}
	got, err := generateSplits([]artifact{timings[0], part}, run, map[string]int{"example/big": 2})
	if err != nil {
		t.Fatal(err)
	}
	if tests := got.Packages["example/big"].Tests; len(tests) != 3 || tests["TestB"] != 2 {
		t.Fatalf("merged tests = %v", tests)
	}
	if !reflect.DeepEqual(got.Source.TimingJobs, []string{"unit", "unit-shard"}) {
		t.Fatalf("timing jobs = %v", got.Source.TimingJobs)
	}
}

func TestGenerateSplitsRejectsUnverifiableInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*[]artifact, *runMetadata)
		split  string
		want   string
	}{
		{name: "branch", mutate: func(_ *[]artifact, run *runMetadata) { run.HeadBranch = "feature" }, want: "on main"},
		{name: "conclusion", mutate: func(_ *[]artifact, run *runMetadata) { run.Conclusion = "failure" }, want: "successful"},
		{name: "timestamp", mutate: func(_ *[]artifact, run *runMetadata) { run.UpdatedAt = "yesterday" }, want: "RFC 3339"},
		{name: "failed test", split: "example/other", want: "non-success"},
		{name: "unmeasured package", split: "example/none", want: "no top-level tests"},
		{name: "mixed platforms", mutate: func(timings *[]artifact, _ *runMetadata) {
			other := (*timings)[0]
			other.Platform = "darwin"
			*timings = append(*timings, other)
		}, want: "mix platforms"},
		{name: "duplicate test", mutate: func(timings *[]artifact, _ *runMetadata) {
			*timings = append(*timings, (*timings)[0])
		}, want: "measured twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timings, run := validSplitInputs()
			if tt.mutate != nil {
				tt.mutate(&timings, &run)
			}
			split := tt.split
			if split == "" {
				split = "example/big"
			}
			_, err := generateSplits(timings, run, map[string]int{split: 2})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParseSplitSpecs(t *testing.T) {
	got, err := parseSplitSpecs([]string{"a/b=3", " c = 2 "})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]int{"a/b": 3, "c": 2}) {
		t.Fatalf("specs = %v", got)
	}
	for _, bad := range [][]string{{"a"}, {"a=1"}, {"=2"}, {"a=x"}, {"a=2", "a=3"}} {
		if _, err := parseSplitSpecs(bad); err == nil {
			t.Errorf("parseSplitSpecs(%q) accepted", bad)
		}
	}
}

func TestRunSplitsWritesTheTable(t *testing.T) {
	dir := t.TempDir()
	timings, run := validSplitInputs()
	timingPath := filepath.Join(dir, "unit.json")
	runPath := filepath.Join(dir, "run.json")
	outPath := filepath.Join(dir, "splits.json")
	writeSplitJSON(t, timingPath, timings[0])
	writeSplitJSON(t, runPath, run)
	var stdout, stderr bytes.Buffer
	code := runTiming(t, []string{"splits", "-timing", timingPath, "-run-metadata", runPath, "-split", "example/big=2", "-out", outPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var document splitDocument
	if err := readJSONFile(outPath, &document); err != nil {
		t.Fatal(err)
	}
	if document.Packages["example/big"].Pieces != 2 {
		t.Fatalf("written document = %+v", document)
	}
	if code := runTiming(t, []string{"splits", "-out", outPath}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing flags exit = %d, want 2", code)
	}
}

func runTiming(t *testing.T, args []string, stdout, stderr *bytes.Buffer) int {
	t.Helper()
	return run(args, stdout, stderr, nil)
}

func writeSplitJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
