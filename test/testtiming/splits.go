package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// splitDocument is .github/unit-shard-splits.json: the packages the Linux race
// shards run in pieces, with per-test seconds used to balance the pieces. The
// hermetic runner (test/hermetic/split.go) is the consumer.
type splitDocument struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Source        splitSource             `json:"source"`
	Packages      map[string]splitPackage `json:"packages"`
}

type splitSource struct {
	Run         int64    `json:"run"`
	Commit      string   `json:"commit"`
	GeneratedAt string   `json:"generatedAt"`
	TimingJobs  []string `json:"timingJobs"`
	Platform    string   `json:"platform"`
}

type splitPackage struct {
	Pieces int                `json:"pieces"`
	Tests  map[string]float64 `json:"tests"`
}

// runMetadata is the subset of the GitHub Actions workflow-run API object the
// split table records as provenance.
type runMetadata struct {
	ID         int64  `json:"id"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	UpdatedAt  string `json:"updated_at"`
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func runSplits(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("splits", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var timingPaths, splitSpecs stringList
	flags.Var(&timingPaths, "timing", "unit timing artifact JSON (repeatable; race-shard parts merge)")
	flags.Var(&splitSpecs, "split", "package=pieces (repeatable)")
	runMetadataPath := flags.String("run-metadata", "", "GitHub Actions workflow run API JSON")
	outputPath := flags.String("out", "", "split table output path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(timingPaths) == 0 || len(splitSpecs) == 0 || *runMetadataPath == "" || *outputPath == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "testtiming splits: -timing, -split, -run-metadata, and -out are required; positional arguments are not accepted")
		return 2
	}
	pieces, err := parseSplitSpecs(splitSpecs)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming splits: %v\n", err)
		return 2
	}
	timings := make([]artifact, len(timingPaths))
	for index, path := range timingPaths {
		if err := readJSONFile(path, &timings[index]); err != nil {
			_, _ = fmt.Fprintf(stderr, "testtiming splits: read timing artifact %s: %v\n", path, err)
			return 1
		}
	}
	var run runMetadata
	if err := readJSONFile(*runMetadataPath, &run); err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming splits: read run metadata: %v\n", err)
		return 1
	}
	document, err := generateSplits(timings, run, pieces)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming splits: %v\n", err)
		return 1
	}
	if err := writeJSONDocument(*outputPath, document); err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming splits: write %s: %v\n", *outputPath, err)
		return 1
	}
	for _, pkg := range sortedKeys(document.Packages) {
		_, _ = fmt.Fprintf(stdout, "%s: %d pieces over %d measured tests\n", pkg, document.Packages[pkg].Pieces, len(document.Packages[pkg].Tests))
	}
	_, _ = fmt.Fprintf(stdout, "wrote split table from run %d (%s) to %s\n", run.ID, run.HeadSHA, *outputPath)
	return 0
}

func parseSplitSpecs(specs []string) (map[string]int, error) {
	pieces := make(map[string]int, len(specs))
	for _, spec := range specs {
		pkg, count, ok := strings.Cut(spec, "=")
		pkg = strings.TrimSpace(pkg)
		value, err := strconv.Atoi(strings.TrimSpace(count))
		if !ok || pkg == "" || err != nil || value < 2 {
			return nil, fmt.Errorf("-split %q must be package=pieces with pieces >= 2", spec)
		}
		if _, duplicate := pieces[pkg]; duplicate {
			return nil, fmt.Errorf("-split names %s twice", pkg)
		}
		pieces[pkg] = value
	}
	return pieces, nil
}

func generateSplits(timings []artifact, run runMetadata, pieces map[string]int) (splitDocument, error) {
	updatedAt, err := validateRunProvenance(run)
	if err != nil {
		return splitDocument{}, err
	}
	platform, jobs, err := timingIdentity(timings)
	if err != nil {
		return splitDocument{}, err
	}
	packages := make(map[string]splitPackage, len(pieces))
	for pkg, count := range pieces {
		tests, err := topLevelTests(timings, pkg)
		if err != nil {
			return splitDocument{}, err
		}
		packages[pkg] = splitPackage{Pieces: count, Tests: tests}
	}
	return splitDocument{
		SchemaVersion: schemaVersion,
		Source: splitSource{
			Run:         run.ID,
			Commit:      run.HeadSHA,
			GeneratedAt: updatedAt.Format(time.RFC3339),
			TimingJobs:  jobs,
			Platform:    platform,
		},
		Packages: packages,
	}, nil
}

func validateRunProvenance(run runMetadata) (time.Time, error) {
	if run.ID <= 0 || run.HeadBranch != "main" || !validCommitSHA(run.HeadSHA) {
		return time.Time{}, errors.New("run metadata: want a positive run ID on main with a full commit SHA")
	}
	if run.Status != "completed" || run.Conclusion != "success" {
		return time.Time{}, errors.New("run metadata: want a completed successful run")
	}
	updatedAt, err := time.Parse(time.RFC3339, run.UpdatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("run metadata: updated_at %q must be RFC 3339", run.UpdatedAt)
	}
	return updatedAt, nil
}

// timingIdentity requires every artifact to come from one platform, and
// records which timing jobs contributed (the coverage job's "unit", or the
// race shards' "unit-shard").
func timingIdentity(timings []artifact) (string, []string, error) {
	platform := ""
	jobs := map[string]struct{}{}
	for _, timing := range timings {
		if timing.SchemaVersion != schemaVersion || strings.TrimSpace(timing.Job) == "" || timing.Platform == "" {
			return "", nil, errors.New("timing artifact: want a current-schema artifact with a job and platform")
		}
		if platform != "" && timing.Platform != platform {
			return "", nil, fmt.Errorf("timing artifacts mix platforms %q and %q", platform, timing.Platform)
		}
		platform = timing.Platform
		jobs[timing.Job] = struct{}{}
	}
	return platform, sortedKeys(jobs), nil
}

// topLevelTests collects pkg's measured top-level tests (subtests are folded
// into their parent's elapsed time by go test) across every artifact.
func topLevelTests(timings []artifact, pkg string) (map[string]float64, error) {
	tests := make(map[string]float64)
	for _, timing := range timings {
		for _, measured := range timing.Tests {
			if measured.Package != pkg || strings.Contains(measured.Test, "/") {
				continue
			}
			if measured.Status != "pass" && measured.Status != "skip" {
				return nil, fmt.Errorf("timing artifact: %s %s has non-success status %q", pkg, measured.Test, measured.Status)
			}
			if !validNonNegative(measured.ElapsedSeconds) {
				return nil, fmt.Errorf("timing artifact: %s %s has invalid elapsedSeconds", pkg, measured.Test)
			}
			if _, duplicate := tests[measured.Test]; duplicate {
				return nil, fmt.Errorf("timing artifact: %s %s measured twice", pkg, measured.Test)
			}
			tests[measured.Test] = math.Round(measured.ElapsedSeconds*100) / 100
		}
	}
	if len(tests) == 0 {
		return nil, fmt.Errorf("timing artifact: no top-level tests measured for %s", pkg)
	}
	return tests, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
