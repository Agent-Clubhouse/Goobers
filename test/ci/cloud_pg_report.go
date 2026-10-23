package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const modulePath = "github.com/goobers/goobers"

// testEvent is one line of `go test -json`.
type testEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

type packageResult struct {
	passed, skipped, failed int
	buildFailed             bool
	output                  map[string]*strings.Builder
	failedTests             []string
}

// cloudPGRoots are the trees the cloud-pg job tests; every package in them
// with PostgreSQL-tagged tests (cloudintegration or cloudpg) must run.
var cloudPGRoots = []string{"internal/cloud", "test/ha/invariants"}

// runCloudPGReport reads the cloud-pg job's `go test -json` stream, prints
// failing tests' output, and fails when any package with PostgreSQL-tagged
// tests under cloudPGRoots ran no passing test (missing, build failure, zero
// tests, or skips only). A missing tag or environment variable then fails the
// job instead of passing silently.
func runCloudPGReport(reportPath, root string, stdout, stderr io.Writer) int {
	file, err := os.Open(reportPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cloud-pg-report: %v\n", err)
		return 2
	}
	defer func() { _ = file.Close() }()
	results, err := readTestEvents(file)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cloud-pg-report: %v\n", err)
		return 2
	}
	required, err := cloudIntegrationPackages(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cloud-pg-report: %v\n", err)
		return 2
	}
	printFailures(results, stdout)
	problems := cloudPGProblems(results, required)
	for _, problem := range problems {
		_, _ = fmt.Fprintf(stderr, "cloud-pg-report: %s\n", problem)
	}
	if len(problems) > 0 {
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "cloud-pg-report: %d PostgreSQL test packages each ran passing tests\n", len(required))
	return 0
}

func readTestEvents(r io.Reader) (map[string]*packageResult, error) {
	results := make(map[string]*packageResult)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var event testEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Package == "" {
			continue // build output and other non-event lines
		}
		result := results[event.Package]
		if result == nil {
			result = &packageResult{output: make(map[string]*strings.Builder)}
			results[event.Package] = result
		}
		switch event.Action {
		case "output":
			if result.output[event.Test] == nil {
				result.output[event.Test] = &strings.Builder{}
			}
			result.output[event.Test].WriteString(event.Output)
		case "pass", "skip", "fail":
			result.record(event)
		}
	}
	return results, scanner.Err()
}

func (r *packageResult) record(event testEvent) {
	switch {
	case event.Test == "" && event.Action == "fail":
		r.buildFailed = true
	case event.Test == "" || strings.Contains(event.Test, "/"):
		// Package summaries and subtests do not count as top-level tests.
	case event.Action == "pass":
		r.passed++
	case event.Action == "skip":
		r.skipped++
	case event.Action == "fail":
		r.failed++
	}
	if event.Action == "fail" && event.Test != "" {
		r.failedTests = append(r.failedTests, event.Test)
	}
}

// cloudIntegrationPackages lists the import paths under cloudPGRoots that
// have test files built only with the cloudintegration or cloudpg tag.
func cloudIntegrationPackages(root string) ([]string, error) {
	seen := make(map[string]bool)
	for _, tree := range cloudPGRoots {
		if err := collectTaggedPackages(root, filepath.Join(root, filepath.FromSlash(tree)), seen); err != nil {
			return nil, err
		}
	}
	packages := make([]string, 0, len(seen))
	for pkg := range seen {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	return packages, nil
}

func collectTaggedPackages(root, base string, seen map[string]bool) error {
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(base, func(file string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(file, "_test.go") {
			return err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		firstLine, _, _ := strings.Cut(string(data), "\n")
		if strings.HasPrefix(firstLine, "//go:build") &&
			(strings.Contains(firstLine, "cloudintegration") || strings.Contains(firstLine, "cloudpg")) {
			relative, err := filepath.Rel(root, filepath.Dir(file))
			if err != nil {
				return err
			}
			seen[path.Join(modulePath, filepath.ToSlash(relative))] = true
		}
		return nil
	})
}

func cloudPGProblems(results map[string]*packageResult, required []string) []string {
	var problems []string
	for _, pkg := range required {
		result := results[pkg]
		switch {
		case result == nil:
			problems = append(problems, pkg+": did not run")
		case result.buildFailed && result.passed == 0 && result.failed == 0:
			problems = append(problems, pkg+": failed to build or set up")
		case result.passed == 0:
			problems = append(problems, fmt.Sprintf("%s: no test passed (%d skipped, %d failed); is the cloudintegration tag or GOOBERS_TEST_POSTGRES_URL missing?",
				pkg, result.skipped, result.failed))
		}
	}
	return problems
}

func printFailures(results map[string]*packageResult, w io.Writer) {
	packages := make([]string, 0, len(results))
	for pkg := range results {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	for _, pkg := range packages {
		result := results[pkg]
		if result.failed == 0 && !result.buildFailed {
			continue
		}
		tests := append([]string(nil), result.failedTests...)
		if result.buildFailed {
			tests = append(tests, "")
		}
		sort.Strings(tests)
		for _, test := range tests {
			if output := result.output[test]; output != nil {
				_, _ = fmt.Fprintf(w, "--- %s %s\n%s", pkg, test, output.String())
			}
		}
	}
}
