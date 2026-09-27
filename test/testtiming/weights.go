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
	// shardWeightsSchemaVersion 2 is the race-mode table: package seconds come
	// from the Linux race shards' own timing parts, not the non-race coverage
	// job. The hermetic loader rejects any other version, so a table in the old
	// shape fails loudly instead of balancing race shards by non-race times.
	shardWeightsSchemaVersion  = 2
	defaultShardSeconds        = 1.0
	defaultMinimumShardSeconds = 3.0
	// raceTimingJob is the timing job every race shard part records (see
	// test/ci's shardTimingJob), and raceTimingArtifacts names the per-shard
	// artifacts that carry the parts.
	raceTimingJob       = "unit-shard"
	raceTimingArtifacts = "test-timings-race-linux-*"
	raceTimingPlatform  = "linux"
)

type shardWeightDocument struct {
	SchemaVersion  int                `json:"schemaVersion"`
	DefaultSeconds float64            `json:"defaultSeconds"`
	Source         shardWeightSource  `json:"source"`
	Packages       map[string]float64 `json:"packages"`
}

type shardWeightSource struct {
	Run                    int64    `json:"run"`
	Branch                 string   `json:"branch"`
	Commit                 string   `json:"commit"`
	GeneratedAt            string   `json:"generatedAt"`
	TimingJobs             []string `json:"timingJobs"`
	ArtifactPattern        string   `json:"artifactPattern"`
	Platform               string   `json:"platform"`
	Architecture           string   `json:"architecture"`
	MinimumRecordedSeconds float64  `json:"minimumRecordedSeconds"`
}

func runWeights(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("weights", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var timingPaths stringList
	flags.Var(&timingPaths, "timing", "race shard timing part JSON (repeatable; every part of one run)")
	splitsPath := flags.String("splits", "", "split table the run was sharded with (.github/unit-shard-splits.json)")
	runMetadataPath := flags.String("run-metadata", "", "GitHub Actions workflow run API JSON")
	outputPath := flags.String("out", "", "shard weights output path")
	minimumSeconds := flags.Float64("minimum-seconds", defaultMinimumShardSeconds, "minimum package duration to record")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(timingPaths) == 0 || *splitsPath == "" || *runMetadataPath == "" || *outputPath == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "testtiming weights: -timing, -splits, -run-metadata, and -out are required; positional arguments are not accepted")
		return 2
	}
	if !validPositiveSeconds(*minimumSeconds) {
		_, _ = fmt.Fprintln(stderr, "testtiming weights: -minimum-seconds must be finite and positive")
		return 2
	}
	inputs, err := readWeightInputs(timingPaths, *splitsPath, *runMetadataPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming weights: %v\n", err)
		return 1
	}
	document, err := generateShardWeights(inputs, *minimumSeconds)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming weights: %v\n", err)
		return 1
	}
	if err := writeJSONDocument(*outputPath, document); err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming weights: write %s: %v\n", *outputPath, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "wrote %d package weights from %d race timing parts (run %d, %s@%s) to %s\n",
		len(document.Packages), len(inputs.timings), inputs.run.ID, inputs.run.HeadBranch, inputs.run.HeadSHA, *outputPath)
	return 0
}

// weightInputs is everything one weights generation reads: every race timing
// part of one run, the split table that run was sharded with, and the run.
type weightInputs struct {
	timings []artifact
	pieces  map[string]int
	run     runMetadata
}

func readWeightInputs(timingPaths []string, splitsPath, runMetadataPath string) (weightInputs, error) {
	inputs := weightInputs{timings: make([]artifact, len(timingPaths))}
	for index, path := range timingPaths {
		if err := readJSONFile(path, &inputs.timings[index]); err != nil {
			return weightInputs{}, fmt.Errorf("read timing part %s: %w", path, err)
		}
	}
	var splits splitDocument
	if err := readJSONFile(splitsPath, &splits); err != nil {
		return weightInputs{}, fmt.Errorf("read split table %s: %w", splitsPath, err)
	}
	inputs.pieces = make(map[string]int, len(splits.Packages))
	for pkg, split := range splits.Packages {
		if split.Pieces < 2 {
			return weightInputs{}, fmt.Errorf("split table %s: %s must have at least 2 pieces", splitsPath, pkg)
		}
		inputs.pieces[pkg] = split.Pieces
	}
	if err := readJSONFile(runMetadataPath, &inputs.run); err != nil {
		return weightInputs{}, fmt.Errorf("read run metadata: %w", err)
	}
	return inputs, nil
}

// generateShardWeights merges one run's race shard parts into package seconds.
// A whole package runs in exactly one part; a split package runs once per
// piece, and its pieces' seconds sum to the package's weight (the hermetic
// runner divides it back by the piece count when it schedules the pieces).
func generateShardWeights(inputs weightInputs, minimumSeconds float64) (shardWeightDocument, error) {
	if !validPositiveSeconds(minimumSeconds) {
		return shardWeightDocument{}, errors.New("minimum package duration must be finite and positive")
	}
	generatedAt, err := validateRunProvenance(inputs.run)
	if err != nil {
		return shardWeightDocument{}, err
	}
	architecture, err := raceTimingIdentity(inputs.timings)
	if err != nil {
		return shardWeightDocument{}, err
	}
	measured, err := mergeRacePackageSeconds(inputs.timings, inputs.pieces)
	if err != nil {
		return shardWeightDocument{}, err
	}
	packages := make(map[string]float64, len(measured))
	for name, seconds := range measured {
		if seconds >= minimumSeconds {
			// go test reports package elapsed time to millisecond precision;
			// restore that precision after the float sum.
			packages[name] = math.Round(seconds*1000) / 1000
		}
	}
	if len(packages) == 0 {
		return shardWeightDocument{}, fmt.Errorf("timing parts: no passing package measured at least %.3g seconds", minimumSeconds)
	}
	return shardWeightDocument{
		SchemaVersion:  shardWeightsSchemaVersion,
		DefaultSeconds: defaultShardSeconds,
		Source: shardWeightSource{
			Run:                    inputs.run.ID,
			Branch:                 inputs.run.HeadBranch,
			Commit:                 inputs.run.HeadSHA,
			GeneratedAt:            generatedAt.Format(time.RFC3339),
			TimingJobs:             []string{raceTimingJob},
			ArtifactPattern:        raceTimingArtifacts,
			Platform:               raceTimingPlatform,
			Architecture:           architecture,
			MinimumRecordedSeconds: minimumSeconds,
		},
		Packages: packages,
	}, nil
}

// raceTimingIdentity requires every part to be a current-schema race shard
// part from one Linux architecture, and returns that architecture.
func raceTimingIdentity(timings []artifact) (string, error) {
	if len(timings) == 0 {
		return "", errors.New("timing parts: at least one race shard part is required")
	}
	architecture := strings.TrimSpace(timings[0].Architecture)
	for _, timing := range timings {
		if timing.SchemaVersion != schemaVersion || timing.Job != raceTimingJob || timing.Platform != raceTimingPlatform {
			return "", fmt.Errorf("timing parts: want current-schema %s/%s parts, got %q/%q", raceTimingJob, raceTimingPlatform, timing.Job, timing.Platform)
		}
		if architecture == "" || timing.Architecture != architecture {
			return "", fmt.Errorf("timing parts: want one recorded architecture, got %q and %q", architecture, timing.Architecture)
		}
		if !validPositiveSeconds(timing.ElapsedSeconds) {
			return "", errors.New("timing parts: elapsedSeconds must be finite and positive")
		}
	}
	return architecture, nil
}

// mergeRacePackageSeconds sums each package's passing seconds across parts and
// checks it ran exactly as often as it was scheduled: once for a whole
// package, once per piece for a split one. A mismatch means the parts are not
// one complete run sharded with this split table.
func mergeRacePackageSeconds(timings []artifact, pieces map[string]int) (map[string]float64, error) {
	seconds := make(map[string]float64)
	runs := make(map[string]int)
	for _, timing := range timings {
		for _, measured := range timing.Packages {
			name, err := validMeasuredPackage(measured)
			if err != nil {
				return nil, err
			}
			runs[name]++
			if measured.Status == "pass" {
				seconds[name] += measured.ElapsedSeconds
			}
		}
	}
	for name, count := range runs {
		want := max(pieces[name], 1)
		if count != want {
			return nil, fmt.Errorf("timing parts: package %q ran %d times, want %d (one per scheduled piece)", name, count, want)
		}
	}
	for pkg := range pieces {
		if runs[pkg] == 0 {
			return nil, fmt.Errorf("timing parts: split package %q was never measured", pkg)
		}
	}
	return seconds, nil
}

func validMeasuredPackage(measured packageTiming) (string, error) {
	name := strings.TrimSpace(measured.Package)
	if name == "" || (name != "github.com/goobers/goobers" && !strings.HasPrefix(name, "github.com/goobers/goobers/")) {
		return "", fmt.Errorf("timing parts: invalid package %q", measured.Package)
	}
	if measured.Status != "pass" && measured.Status != "skip" {
		return "", fmt.Errorf("timing parts: package %q has non-success status %q", name, measured.Status)
	}
	if !validNonNegative(measured.ElapsedSeconds) {
		return "", fmt.Errorf("timing parts: package %q has invalid elapsedSeconds", name)
	}
	return name, nil
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
