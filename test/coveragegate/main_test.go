package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestFilterProfile(t *testing.T) {
	t.Parallel()
	const profile = `mode: atomic
github.com/goobers/goobers/internal/runner/run.go:10.1,12.2 2 1
github.com/goobers/goobers/cmd/goobers/main.go:20.1,22.2 2 0
github.com/goobers/goobers/api/v1alpha1/zz_generated.deepcopy.go:30.1,32.2 2 0
github.com/goobers/goobers/api/schemas/embed.go:40.1,42.2 2 0
github.com/goobers/goobers/internal/runner/run.go:14.1,16.2 2 0
`
	exclude := regexp.MustCompile(defaultExclude)

	filtered, excluded, err := filterProfile([]byte(profile), exclude)
	if err != nil {
		t.Fatal(err)
	}
	const wantFiltered = `mode: atomic
github.com/goobers/goobers/internal/runner/run.go:10.1,12.2 2 1
github.com/goobers/goobers/internal/runner/run.go:14.1,16.2 2 0
`
	if string(filtered) != wantFiltered {
		t.Fatalf("filtered profile:\n%s\nwant:\n%s", filtered, wantFiltered)
	}
	wantExcluded := []string{
		"github.com/goobers/goobers/api/schemas/embed.go",
		"github.com/goobers/goobers/api/v1alpha1/zz_generated.deepcopy.go",
		"github.com/goobers/goobers/cmd/goobers/main.go",
	}
	if strings.Join(excluded, "\n") != strings.Join(wantExcluded, "\n") {
		t.Fatalf("excluded files = %q, want %q", excluded, wantExcluded)
	}
}

func TestFilterProfileRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		profile string
		want    string
	}{
		{name: "empty", want: "profile is empty"},
		{name: "bad mode", profile: "mode: other\n", want: "invalid mode line"},
		{name: "bad entry", profile: "mode: set\nnot a profile entry\n", want: "line 2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := filterProfile([]byte(test.profile), regexp.MustCompile(defaultExclude))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("filterProfile() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestParseTotalCoverage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		report     string
		wantText   string
		want       float64
		wantErrSub string
	}{
		{
			name:     "decimal total",
			report:   "sample.go:10:\tRun\t75.0%\ntotal:\t(statements)\t72.4%\n",
			wantText: "72.4",
			want:     72.4,
		},
		{
			name:     "integer total",
			report:   "total: (statements) 100%\n",
			wantText: "100",
			want:     100,
		},
		{
			name:       "missing total",
			report:     "sample.go:10:\tRun\t75.0%\n",
			wantErrSub: "could not find",
		},
		{
			name:       "invalid total",
			report:     "total:\t(statements)\tunknown%\n",
			wantErrSub: "could not parse",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			text, got, err := parseTotalCoverage([]byte(test.report))
			if test.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrSub) {
					t.Fatalf("parseTotalCoverage() error = %v, want containing %q", err, test.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if text != test.wantText || got != test.want {
				t.Fatalf("parseTotalCoverage() = %q, %v; want %q, %v", text, got, test.wantText, test.want)
			}
		})
	}
}

func TestBelowThreshold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		total     float64
		threshold float64
		want      bool
	}{
		{name: "below", total: 69.9, threshold: 70, want: true},
		{name: "equal", total: 70, threshold: 70, want: false},
		{name: "above", total: 70.1, threshold: 70, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := belowThreshold(test.total, test.threshold); got != test.want {
				t.Fatalf("belowThreshold(%v, %v) = %v, want %v", test.total, test.threshold, got, test.want)
			}
		})
	}
}

func TestParsePercentageRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "not-a-number", "NaN", "+Inf"} {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			if _, err := parsePercentage(value); err == nil {
				t.Fatalf("parsePercentage(%q) succeeded", value)
			}
		})
	}
}

func TestRunMatchesThresholdBoundary(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(source, []byte("package sample\n\nfunc covered() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(dir, "coverage.out")
	entry := source + ":3.1,3.18 1 1\n"
	if err := os.WriteFile(profile, []byte("mode: set\n"+entry), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COVERAGE_PROFILE", profile)
	t.Setenv("COVERAGE_EXCLUDE", "$^")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"100"}, &stdout, &stderr); code != 0 {
		t.Fatalf("equal threshold exit = %d\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	if !strings.Contains(stdout.String(), "testable-logic coverage: 100.0%") {
		t.Fatalf("pass output missing total:\n%s", &stdout)
	}

	stdout.Reset()
	stderr.Reset()
	t.Setenv("COVERAGE_THRESHOLD", "100.1")
	if code := run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("higher threshold exit = %d\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	if !strings.Contains(stderr.String(), "FAIL: coverage 100.0% is below threshold 100.1%") {
		t.Fatalf("failure output missing decision:\n%s", &stderr)
	}
}
