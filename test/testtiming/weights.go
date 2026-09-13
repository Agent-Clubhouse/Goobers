package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/durability"
)

const (
	defaultShardSeconds        = 1.0
	defaultMinimumShardSeconds = 3.0
	canonicalTimingArtifact    = "test-timings-macOS"
	canonicalTimingJob         = "unit behavioral suite (macos)"
)

type artifactMetadata struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Expired   bool   `json:"expired"`
	Workflow  struct {
		ID         int64  `json:"id"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
	} `json:"workflow_run"`
}

type jobMetadata struct {
	ID          int64  `json:"id"`
	RunID       int64  `json:"run_id"`
	HeadSHA     string `json:"head_sha"`
	HeadBranch  string `json:"head_branch"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
}

type shardWeightDocument struct {
	SchemaVersion  int                `json:"schemaVersion"`
	DefaultSeconds float64            `json:"defaultSeconds"`
	Source         shardWeightSource  `json:"source"`
	Packages       map[string]float64 `json:"packages"`
}

type shardWeightSource struct {
	Run                    int64   `json:"run"`
	Jobs                   []int64 `json:"jobs"`
	Artifact               int64   `json:"artifact"`
	ArtifactName           string  `json:"artifactName"`
	Commit                 string  `json:"commit"`
	GeneratedAt            string  `json:"generatedAt"`
	Platform               string  `json:"platform"`
	Architecture           string  `json:"architecture"`
	MinimumRecordedSeconds float64 `json:"minimumRecordedSeconds"`
}

func runWeights(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("weights", flag.ContinueOnError)
	flags.SetOutput(stderr)
	timingPath := flags.String("timing", "", "unit timing artifact JSON")
	artifactMetadataPath := flags.String("artifact-metadata", "", "GitHub Actions artifact API JSON")
	jobMetadataPath := flags.String("job-metadata", "", "GitHub Actions job API JSON")
	outputPath := flags.String("out", "", "shard weights output path")
	minimumSeconds := flags.Float64("minimum-seconds", defaultMinimumShardSeconds, "minimum package duration to record")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *timingPath == "" || *artifactMetadataPath == "" || *jobMetadataPath == "" || *outputPath == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "testtiming weights: -timing, -artifact-metadata, -job-metadata, and -out are required; positional arguments are not accepted")
		return 2
	}
	if !validPositiveSeconds(*minimumSeconds) {
		_, _ = fmt.Fprintln(stderr, "testtiming weights: -minimum-seconds must be finite and positive")
		return 2
	}

	var timing artifact
	if err := readJSONFile(*timingPath, &timing); err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming weights: read timing artifact: %v\n", err)
		return 1
	}
	var artifactMeta artifactMetadata
	if err := readJSONFile(*artifactMetadataPath, &artifactMeta); err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming weights: read artifact metadata: %v\n", err)
		return 1
	}
	var jobMeta jobMetadata
	if err := readJSONFile(*jobMetadataPath, &jobMeta); err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming weights: read job metadata: %v\n", err)
		return 1
	}
	document, err := generateShardWeights(timing, artifactMeta, jobMeta, *minimumSeconds)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming weights: %v\n", err)
		return 1
	}
	if err := writeJSONDocument(*outputPath, document); err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming weights: write %s: %v\n", *outputPath, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "wrote %d package weights from artifact %d (run %d, job %d, created %s) to %s\n",
		len(document.Packages), artifactMeta.ID, artifactMeta.Workflow.ID, jobMeta.ID, document.Source.GeneratedAt, *outputPath)
	return 0
}

func generateShardWeights(timing artifact, artifactMeta artifactMetadata, jobMeta jobMetadata, minimumSeconds float64) (shardWeightDocument, error) {
	if timing.SchemaVersion != schemaVersion {
		return shardWeightDocument{}, fmt.Errorf("timing artifact: unsupported schemaVersion %d", timing.SchemaVersion)
	}
	if timing.Job != "unit" || timing.Platform != "darwin" || strings.TrimSpace(timing.Architecture) == "" {
		return shardWeightDocument{}, fmt.Errorf("timing artifact: want unit/darwin with a recorded architecture, got %q/%q/%q", timing.Job, timing.Platform, timing.Architecture)
	}
	if !validPositiveSeconds(timing.ElapsedSeconds) {
		return shardWeightDocument{}, errors.New("timing artifact: elapsedSeconds must be finite and positive")
	}
	if !validPositiveSeconds(minimumSeconds) {
		return shardWeightDocument{}, errors.New("minimum package duration must be finite and positive")
	}
	createdAt, err := validateWeightProvenance(artifactMeta, jobMeta)
	if err != nil {
		return shardWeightDocument{}, err
	}

	packages := make(map[string]float64)
	seen := make(map[string]struct{}, len(timing.Packages))
	for _, measured := range timing.Packages {
		name := strings.TrimSpace(measured.Package)
		if name == "" || (name != "github.com/goobers/goobers" && !strings.HasPrefix(name, "github.com/goobers/goobers/")) {
			return shardWeightDocument{}, fmt.Errorf("timing artifact: invalid package %q", measured.Package)
		}
		if _, duplicate := seen[name]; duplicate {
			return shardWeightDocument{}, fmt.Errorf("timing artifact: duplicate package %q", name)
		}
		seen[name] = struct{}{}
		if measured.Status != "pass" && measured.Status != "skip" {
			return shardWeightDocument{}, fmt.Errorf("timing artifact: package %q has non-success status %q", name, measured.Status)
		}
		if measured.ElapsedSeconds < 0 || math.IsInf(measured.ElapsedSeconds, 0) || math.IsNaN(measured.ElapsedSeconds) {
			return shardWeightDocument{}, fmt.Errorf("timing artifact: package %q has invalid elapsedSeconds", name)
		}
		if measured.Status == "pass" && measured.ElapsedSeconds >= minimumSeconds {
			// go test reports package elapsed time to millisecond precision. Some
			// JSON decoders expose that decimal as a longer binary float; restore
			// the measurement's actual precision in the checked-in document.
			packages[name] = math.Round(measured.ElapsedSeconds*1000) / 1000
		}
	}
	if len(packages) == 0 {
		return shardWeightDocument{}, fmt.Errorf("timing artifact: no passing package measured at least %.3g seconds", minimumSeconds)
	}

	return shardWeightDocument{
		SchemaVersion:  schemaVersion,
		DefaultSeconds: defaultShardSeconds,
		Source: shardWeightSource{
			Run:                    artifactMeta.Workflow.ID,
			Jobs:                   []int64{jobMeta.ID},
			Artifact:               artifactMeta.ID,
			ArtifactName:           artifactMeta.Name,
			Commit:                 artifactMeta.Workflow.HeadSHA,
			GeneratedAt:            createdAt.Format(time.RFC3339),
			Platform:               timing.Platform,
			Architecture:           timing.Architecture,
			MinimumRecordedSeconds: minimumSeconds,
		},
		Packages: packages,
	}, nil
}

func validateWeightProvenance(artifactMeta artifactMetadata, jobMeta jobMetadata) (time.Time, error) {
	if artifactMeta.ID <= 0 || artifactMeta.Name != canonicalTimingArtifact || artifactMeta.Expired {
		return time.Time{}, fmt.Errorf("artifact metadata: want an unexpired positive-ID %q artifact", canonicalTimingArtifact)
	}
	if artifactMeta.Workflow.ID <= 0 || artifactMeta.Workflow.HeadBranch != "main" || !validCommitSHA(artifactMeta.Workflow.HeadSHA) {
		return time.Time{}, errors.New("artifact metadata: workflow run must identify main with a full commit SHA")
	}
	if jobMeta.ID <= 0 || jobMeta.RunID != artifactMeta.Workflow.ID || jobMeta.HeadSHA != artifactMeta.Workflow.HeadSHA || jobMeta.HeadBranch != artifactMeta.Workflow.HeadBranch {
		return time.Time{}, errors.New("job metadata: run, branch, and commit must match the artifact metadata")
	}
	if jobMeta.Name != canonicalTimingJob || jobMeta.Status != "completed" || jobMeta.Conclusion != "success" {
		return time.Time{}, fmt.Errorf("job metadata: want completed successful job %q", canonicalTimingJob)
	}
	createdAt, err := time.Parse(time.RFC3339, artifactMeta.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("artifact metadata: created_at %q must be RFC 3339", artifactMeta.CreatedAt)
	}
	startedAt, startErr := time.Parse(time.RFC3339, jobMeta.StartedAt)
	completedAt, completeErr := time.Parse(time.RFC3339, jobMeta.CompletedAt)
	if startErr != nil || completeErr != nil || completedAt.Before(startedAt) {
		return time.Time{}, errors.New("job metadata: started_at and completed_at must be a valid ordered RFC 3339 window")
	}
	if createdAt.Before(startedAt) || createdAt.After(completedAt) {
		return time.Time{}, errors.New("artifact metadata: created_at must fall within the producing job window")
	}
	return createdAt, nil
}

func validCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validPositiveSeconds(value float64) bool {
	return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

func readJSONFile(path string, destination any) (returnErr error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON document has trailing content")
	}
	return nil
}

func writeJSONDocument(path string, value any) error {
	return writeJSONDocumentWithReplace(path, value, durability.ReplaceFile)
}

func writeJSONDocumentWithReplace(path string, value any, replace func(string, string) error) error {
	var content bytes.Buffer
	encoder := json.NewEncoder(&content)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}

	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err := file.Write(content.Bytes()); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Chmod(0o644); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := replace(temporary, path); err != nil {
		return err
	}
	return durability.SyncDir(directory)
}
