package main

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	examplePackage = "github.com/goobers/goobers/internal/example"
	splitPackageA  = "github.com/goobers/goobers/cmd/big"
)

// validRaceWeightInputs is one run's worth of race shard parts: two shards'
// whole packages, plus a package split into two pieces, each measured by its
// own part as the hermetic runner records it.
func validRaceWeightInputs() weightInputs {
	part := func(packages ...packageTiming) artifact {
		return artifact{SchemaVersion: schemaVersion, Job: raceTimingJob, Platform: "linux", Architecture: "amd64", ElapsedSeconds: 100, Packages: packages}
	}
	return weightInputs{
		timings: []artifact{
			part(
				packageTiming{Package: examplePackage, Status: "pass", ElapsedSeconds: 4},
				packageTiming{Package: "github.com/goobers/goobers/internal/rounded", Status: "pass", ElapsedSeconds: 3.14159},
			),
			part(packageTiming{Package: splitPackageA, Status: "pass", ElapsedSeconds: 200.5}),
			part(
				packageTiming{Package: "github.com/goobers/goobers/internal/small", Status: "pass", ElapsedSeconds: 2.999},
				packageTiming{Package: "github.com/goobers/goobers", Status: "skip", ElapsedSeconds: 0},
			),
			part(packageTiming{Package: splitPackageA, Status: "pass", ElapsedSeconds: 100.25}),
		},
		pieces: map[string]int{splitPackageA: 2},
		run: runMetadata{
			ID: 42, HeadBranch: "main", HeadSHA: strings.Repeat("c", 40),
			Status: "completed", Conclusion: "success", UpdatedAt: "2026-09-25T00:19:09Z",
		},
	}
}

func TestGenerateShardWeightsSumsSplitPiecesAndRecordsRaceProvenance(t *testing.T) {
	inputs := validRaceWeightInputs()
	got, err := generateShardWeights(inputs, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := shardWeightDocument{
		SchemaVersion:  shardWeightsSchemaVersion,
		DefaultSeconds: defaultShardSeconds,
		Source: shardWeightSource{
			Run: 42, Branch: "main", Commit: strings.Repeat("c", 40), GeneratedAt: "2026-09-25T00:19:09Z",
			TimingJobs: []string{"unit-shard"}, ArtifactPattern: "test-timings-race-linux-*",
			Platform: "linux", Architecture: "amd64", MinimumRecordedSeconds: 3,
		},
		Packages: map[string]float64{
			examplePackage: 4,
			"github.com/goobers/goobers/internal/rounded": 3.142,
			splitPackageA: 300.75,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("weights = %+v\nwant %+v", got, want)
	}
}

func TestGenerateShardWeightsRejectsIncompleteOrForeignParts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*weightInputs)
		want   string
	}{
		{name: "failed run", mutate: func(in *weightInputs) { in.run.Conclusion = "failure" }, want: "completed successful"},
		{name: "short commit", mutate: func(in *weightInputs) { in.run.HeadSHA = "abc" }, want: "full commit SHA"},
		{name: "coverage job part", mutate: func(in *weightInputs) { in.timings[0].Job = "unit" }, want: "unit-shard/linux"},
		{name: "other platform", mutate: func(in *weightInputs) { in.timings[1].Platform = "darwin" }, want: "unit-shard/linux"},
		{name: "mixed architecture", mutate: func(in *weightInputs) { in.timings[2].Architecture = "arm64" }, want: "one recorded architecture"},
		{name: "failed package", mutate: func(in *weightInputs) { in.timings[0].Packages[0].Status = "fail" }, want: "non-success status"},
		{name: "foreign package", mutate: func(in *weightInputs) { in.timings[0].Packages[0].Package = "example.com/x" }, want: "invalid package"},
		{name: "whole package twice", mutate: func(in *weightInputs) {
			in.timings[2].Packages = append(in.timings[2].Packages, in.timings[0].Packages[0])
		}, want: "ran 2 times, want 1"},
		{name: "missing piece", mutate: func(in *weightInputs) { in.timings = in.timings[:3] }, want: "ran 1 times, want 2"},
		{name: "pieces changed", mutate: func(in *weightInputs) { in.pieces[splitPackageA] = 3 }, want: "ran 2 times, want 3"},
		{name: "split never measured", mutate: func(in *weightInputs) { in.pieces["github.com/goobers/goobers/gone"] = 2 }, want: "never measured"},
		{name: "no parts", mutate: func(in *weightInputs) { in.timings = nil }, want: "at least one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputs := validRaceWeightInputs()
			tt.mutate(&inputs)
			_, err := generateShardWeights(inputs, 3)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestRunWeightsWritesDeterministicDocument(t *testing.T) {
	inputs := validRaceWeightInputs()
	directory := t.TempDir()
	args := []string{"weights"}
	for index, timing := range inputs.timings {
		path := filepath.Join(directory, "unit-race.part"+string(rune('1'+index))+".json")
		writeJSONFile(t, path, timing)
		args = append(args, "-timing", path)
	}
	splitsPath := filepath.Join(directory, "splits.json")
	writeJSONFile(t, splitsPath, splitDocument{SchemaVersion: schemaVersion, Packages: map[string]splitPackage{splitPackageA: {Pieces: 2}}})
	runPath := filepath.Join(directory, "run.json")
	writeJSONFile(t, runPath, inputs.run)
	args = append(args, "-splits", splitsPath, "-run-metadata", runPath)

	var first []byte
	for index := 0; index < 2; index++ {
		output := filepath.Join(directory, "weights-"+string(rune('a'+index))+".json")
		var stdout, stderr bytes.Buffer
		code := run(append(append([]string(nil), args...), "-out", output), &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("run = %d, stderr = %s", code, &stderr)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = data
		} else if !bytes.Equal(first, data) {
			t.Fatalf("same inputs produced different output:\n%s\n%s", first, data)
		}
		info, err := os.Stat(output)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Fatalf("output mode = %o, want 644", got)
		}
	}
}

func TestRunWeightsRequiresTheSplitTable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"weights", "-timing", "a.json", "-run-metadata", "run.json", "-out", "w.json"}, &stdout, &stderr, nil); code != 2 {
		t.Fatalf("run without -splits = %d, want usage error 2", code)
	}
}

func TestGenerateShardWeightsRejectsInvalidMinimum(t *testing.T) {
	for _, minimum := range []float64{0, -1, math.Inf(1), math.NaN()} {
		if _, err := generateShardWeights(validRaceWeightInputs(), minimum); err == nil {
			t.Fatalf("minimum %v was accepted", minimum)
		}
	}
}

func TestWriteJSONDocumentDoesNotDamageDestinationOnEncodingFailure(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "weights.json")
	want := []byte("existing weights\n")
	if err := os.WriteFile(destination, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONDocument(destination, map[string]any{"unsupported": make(chan int)}); err == nil {
		t.Fatal("writeJSONDocument accepted a value JSON cannot encode")
	}
	assertUnchangedDestinationAndNoTemps(t, destination, want)
}

func TestWriteJSONDocumentDoesNotDamageDestinationOnReplaceFailure(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "weights.json")
	want := []byte("existing weights\n")
	if err := os.WriteFile(destination, want, 0o644); err != nil {
		t.Fatal(err)
	}
	replaceErr := errors.New("injected replace failure")
	err := writeJSONDocumentWithReplace(destination, map[string]int{"new": 1}, func(_, _ string) error { return replaceErr })
	if !errors.Is(err, replaceErr) {
		t.Fatalf("writeJSONDocumentWithReplace error = %v, want %v", err, replaceErr)
	}
	assertUnchangedDestinationAndNoTemps(t, destination, want)
}

func assertUnchangedDestinationAndNoTemps(t *testing.T, destination string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("destination = %q, want original %q", got, want)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("failed write left temporary files: %v", temps)
	}
}
