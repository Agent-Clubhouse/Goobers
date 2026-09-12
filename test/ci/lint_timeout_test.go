package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type lintTimingEvidence struct {
	Source  string             `json:"source"`
	Samples []lintTimingSample `json:"samples"`
}

type lintTimingSample struct {
	RunID       int64  `json:"runId"`
	JobID       int64  `json:"jobId"`
	HeadSHA     string `json:"headSHA"`
	GOOS        string `json:"goos"`
	Job         string `json:"job"`
	Step        string `json:"step"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
}

func TestLintTimeoutHasMeasuredMargin(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	evidence := loadLintTimingEvidence(t, filepath.Join(root, "test", "ci", "lint-timing-samples.json"))
	durations := validateLintTimingEvidence(t, evidence)

	configData, err := os.ReadFile(filepath.Join(root, ".golangci.yml"))
	if err != nil {
		t.Fatalf("read golangci-lint config: %v", err)
	}
	var config struct {
		Run struct {
			Timeout string `yaml:"timeout"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		t.Fatalf("parse golangci-lint config: %v", err)
	}
	timeout, err := time.ParseDuration(config.Run.Timeout)
	if err != nil {
		t.Fatalf("parse run.timeout %q: %v", config.Run.Timeout, err)
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median := durations[len(durations)/2]
	const requiredMargin = 3
	if timeout < requiredMargin*median {
		t.Fatalf("golangci-lint timeout %s is below %dx measured median %s (need at least %s)", timeout, requiredMargin, median, requiredMargin*median)
	}
}

func loadLintTimingEvidence(t *testing.T, path string) lintTimingEvidence {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lint timing evidence: %v", err)
	}
	var evidence lintTimingEvidence
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		t.Fatalf("parse lint timing evidence: %v", err)
	}
	return evidence
}

func validateLintTimingEvidence(t *testing.T, evidence lintTimingEvidence) []time.Duration {
	t.Helper()
	const source = "https://api.github.com/repos/Agent-Clubhouse/Goobers/actions/runs/{runId}/jobs"
	if evidence.Source != source {
		t.Fatalf("lint timing source = %q, want authoritative jobs API %q", evidence.Source, source)
	}
	if len(evidence.Samples) < 15 || len(evidence.Samples)%2 == 0 {
		t.Fatalf("lint timing sample count = %d, want an odd reviewed set of at least 15", len(evidence.Samples))
	}

	wantPlatforms := map[string]bool{"linux": true, "darwin": true, "windows": true}
	runs := make(map[int64]map[string]string)
	durations := make([]time.Duration, 0, len(evidence.Samples))
	for _, sample := range evidence.Samples {
		if sample.RunID <= 0 || sample.JobID <= 0 {
			t.Fatalf("sample has invalid run/job identity: %#v", sample)
		}
		if len(sample.HeadSHA) != 40 || strings.Trim(sample.HeadSHA, "0123456789abcdef") != "" {
			t.Fatalf("run %d has invalid head SHA %q", sample.RunID, sample.HeadSHA)
		}
		if !wantPlatforms[sample.GOOS] {
			t.Fatalf("run %d has unsupported GOOS %q", sample.RunID, sample.GOOS)
		}
		if sample.Job != fmt.Sprintf("lint (%s)", sample.GOOS) || sample.Step != fmt.Sprintf("golangci-lint (GOOS=%s)", sample.GOOS) {
			t.Fatalf("run %d %s does not identify the lint job/step: job=%q step=%q", sample.RunID, sample.GOOS, sample.Job, sample.Step)
		}
		if sample.Conclusion != "success" {
			t.Fatalf("run %d %s conclusion = %q, want success", sample.RunID, sample.GOOS, sample.Conclusion)
		}
		started, err := time.Parse(time.RFC3339, sample.StartedAt)
		if err != nil {
			t.Fatalf("run %d %s startedAt: %v", sample.RunID, sample.GOOS, err)
		}
		completed, err := time.Parse(time.RFC3339, sample.CompletedAt)
		if err != nil {
			t.Fatalf("run %d %s completedAt: %v", sample.RunID, sample.GOOS, err)
		}
		duration := completed.Sub(started)
		if duration <= 0 {
			t.Fatalf("run %d %s has non-positive duration %s", sample.RunID, sample.GOOS, duration)
		}
		durations = append(durations, duration)

		platforms := runs[sample.RunID]
		if platforms == nil {
			platforms = make(map[string]string)
			runs[sample.RunID] = platforms
		}
		if previous, duplicate := platforms[sample.GOOS]; duplicate {
			t.Fatalf("run %d repeats GOOS %s (heads %s and %s)", sample.RunID, sample.GOOS, previous, sample.HeadSHA)
		}
		platforms[sample.GOOS] = sample.HeadSHA
	}
	if len(runs) < 5 {
		t.Fatalf("lint timing evidence covers %d runs, want at least 5", len(runs))
	}
	for runID, platforms := range runs {
		if len(platforms) != len(wantPlatforms) {
			t.Fatalf("run %d covers platforms %v, want linux, darwin, and windows", runID, platforms)
		}
		var head string
		for platform, current := range platforms {
			if head != "" && current != head {
				t.Fatalf("run %d mixes head SHAs: %s has %s, prior %s", runID, platform, current, head)
			}
			head = current
		}
	}
	return durations
}

func TestLintWorkflowRunsTimeoutInvariantBeforeLint(t *testing.T) {
	t.Parallel()
	job := loadCIWorkflow(t).Jobs["lint"]
	marginIndex := job.stepIndex("Validate lint timeout margin")
	lintIndex := job.stepIndex("golangci-lint (GOOS=${{ matrix.goos }})")
	if marginIndex < 0 || lintIndex < 0 || marginIndex >= lintIndex {
		t.Fatalf("lint step order = %v; timeout invariant must run before golangci-lint", job.stepNames())
	}
	if got := job.Steps[marginIndex].Run; got != "go test ./test/ci -run ^TestLintTimeoutHasMeasuredMargin$ -count=1" {
		t.Fatalf("lint timeout invariant command = %q", got)
	}
}
