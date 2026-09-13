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

func TestGenerateShardWeightsUsesArtifactTimestampAndMeasuredThreshold(t *testing.T) {
	timing, artifactMeta, jobMeta := validWeightInputs()
	timing.Packages = []packageTiming{
		{Package: "github.com/goobers/goobers/internal/z", Status: "pass", ElapsedSeconds: 3},
		{Package: "github.com/goobers/goobers/internal/rounded", Status: "pass", ElapsedSeconds: 3.14159},
		{Package: "github.com/goobers/goobers/internal/a", Status: "pass", ElapsedSeconds: 2.999},
		{Package: "github.com/goobers/goobers", Status: "skip", ElapsedSeconds: 0},
	}

	got, err := generateShardWeights(timing, artifactMeta, jobMeta, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source.GeneratedAt != artifactMeta.CreatedAt {
		t.Fatalf("generatedAt = %q, want artifact created_at %q", got.Source.GeneratedAt, artifactMeta.CreatedAt)
	}
	if got.Source.Run != artifactMeta.Workflow.ID || !reflect.DeepEqual(got.Source.Jobs, []int64{jobMeta.ID}) ||
		got.Source.Artifact != artifactMeta.ID || got.Source.Commit != artifactMeta.Workflow.HeadSHA {
		t.Fatalf("source provenance = %+v", got.Source)
	}
	want := map[string]float64{
		"github.com/goobers/goobers/internal/rounded": 3.142,
		"github.com/goobers/goobers/internal/z":       3,
	}
	if !reflect.DeepEqual(got.Packages, want) {
		t.Fatalf("packages = %#v, want measurements at the inclusive threshold rounded to milliseconds", got.Packages)
	}
}

func TestGenerateShardWeightsRejectsUnverifiableInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*artifact, *artifactMetadata, *jobMetadata)
		want   string
	}{
		{name: "wrong branch", mutate: func(_ *artifact, meta *artifactMetadata, _ *jobMetadata) { meta.Workflow.HeadBranch = "feature" }, want: "identify main"},
		{name: "run mismatch", mutate: func(_ *artifact, _ *artifactMetadata, job *jobMetadata) { job.RunID++ }, want: "must match"},
		{name: "commit mismatch", mutate: func(_ *artifact, _ *artifactMetadata, job *jobMetadata) { job.HeadSHA = strings.Repeat("b", 40) }, want: "must match"},
		{name: "failed job", mutate: func(_ *artifact, _ *artifactMetadata, job *jobMetadata) { job.Conclusion = "failure" }, want: "completed successful"},
		{name: "timestamp outside job", mutate: func(_ *artifact, meta *artifactMetadata, _ *jobMetadata) { meta.CreatedAt = "2026-09-12T20:49:00Z" }, want: "within the producing job window"},
		{name: "failed package", mutate: func(timing *artifact, _ *artifactMetadata, _ *jobMetadata) { timing.Packages[0].Status = "fail" }, want: "non-success status"},
		{name: "duplicate package", mutate: func(timing *artifact, _ *artifactMetadata, _ *jobMetadata) {
			timing.Packages = append(timing.Packages, timing.Packages[0])
		}, want: "duplicate package"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timing, artifactMeta, jobMeta := validWeightInputs()
			tt.mutate(&timing, &artifactMeta, &jobMeta)
			_, err := generateShardWeights(timing, artifactMeta, jobMeta, 3)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestRunWeightsWritesDeterministicDocument(t *testing.T) {
	timing, artifactMeta, jobMeta := validWeightInputs()
	directory := t.TempDir()
	timingPath := filepath.Join(directory, "timing.json")
	artifactPath := filepath.Join(directory, "artifact.json")
	jobPath := filepath.Join(directory, "job.json")
	writeJSONFile(t, timingPath, timing)
	writeJSONFile(t, artifactPath, artifactMeta)
	writeJSONFile(t, jobPath, jobMeta)

	var first []byte
	for index := 0; index < 2; index++ {
		output := filepath.Join(directory, "weights-"+string(rune('a'+index))+".json")
		var stdout, stderr bytes.Buffer
		code := run([]string{"weights", "-timing", timingPath, "-artifact-metadata", artifactPath, "-job-metadata", jobPath, "-out", output}, &stdout, &stderr, nil)
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

func TestGenerateShardWeightsRejectsInvalidMinimum(t *testing.T) {
	timing, artifactMeta, jobMeta := validWeightInputs()
	for _, minimum := range []float64{0, -1, math.Inf(1), math.NaN()} {
		if _, err := generateShardWeights(timing, artifactMeta, jobMeta, minimum); err == nil {
			t.Fatalf("minimum %v was accepted", minimum)
		}
	}
}

func validWeightInputs() (artifact, artifactMetadata, jobMetadata) {
	commit := strings.Repeat("a", 40)
	timing := artifact{
		SchemaVersion:  schemaVersion,
		Job:            "unit",
		Platform:       "darwin",
		Architecture:   "arm64",
		ElapsedSeconds: 100,
		Packages: []packageTiming{
			{Package: "github.com/goobers/goobers/internal/example", Status: "pass", ElapsedSeconds: 4},
		},
	}
	artifactMeta := artifactMetadata{ID: 30, Name: canonicalTimingArtifact, CreatedAt: "2026-09-12T20:48:16Z"}
	artifactMeta.Workflow.ID = 10
	artifactMeta.Workflow.HeadBranch = "main"
	artifactMeta.Workflow.HeadSHA = commit
	jobMeta := jobMetadata{
		ID: 20, RunID: 10, HeadSHA: commit, HeadBranch: "main", Name: canonicalTimingJob,
		Status: "completed", Conclusion: "success", StartedAt: "2026-09-12T20:28:44Z", CompletedAt: "2026-09-12T20:48:26Z",
	}
	return timing, artifactMeta, jobMeta
}
