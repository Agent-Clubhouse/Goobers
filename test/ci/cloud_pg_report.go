package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	buildFailed bool
	output      map[string]*strings.Builder
	failedTests []string
	// outcomes includes descendants so a passing parent cannot hide skipped fixtures.
	outcomes map[string]string
}

// cloudPGRoots are the trees the cloud-pg job tests; every package in them
// with PostgreSQL-tagged tests (cloudintegration or cloudpg) must run.
var cloudPGRoots = []string{"internal/cloud", "internal/livejournal", "test/ha/invariants", "test/journalequivalence"}

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
	_, _ = fmt.Fprintf(stdout, "cloud-pg-report: %d PostgreSQL test packages each passed their tagged tests with none skipped\n", len(required))
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
			result = &packageResult{output: make(map[string]*strings.Builder), outcomes: make(map[string]string)}
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
	case event.Test == "":
		// Package summaries do not count as tests.
	default:
		r.outcomes[event.Test] = event.Action
	}
	if event.Action == "fail" && event.Test != "" {
		r.failedTests = append(r.failedTests, event.Test)
	}
}

// cloudIntegrationPackages maps each package under cloudPGRoots to the
// top-level tests defined in its cloudintegration- or cloudpg-tagged files.
// Only those tests prove the PostgreSQL suites ran: a package's untagged unit
// tests pass without any database.
func cloudIntegrationPackages(root string) (map[string][]string, error) {
	tagged := make(map[string][]string)
	for _, tree := range cloudPGRoots {
		if err := collectTaggedTests(root, filepath.Join(root, filepath.FromSlash(tree)), tagged); err != nil {
			return nil, err
		}
	}
	for pkg := range tagged {
		sort.Strings(tagged[pkg])
	}
	return tagged, nil
}

func collectTaggedTests(root, base string, tagged map[string][]string) error {
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
		pgTagged := strings.Contains(firstLine, "cloudintegration") || strings.Contains(firstLine, "cloudpg")
		if !strings.HasPrefix(firstLine, "//go:build") || !pgTagged {
			return nil
		}
		names, err := topLevelTests(file, data)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, filepath.Dir(file))
		if err != nil {
			return err
		}
		pkg := path.Join(modulePath, filepath.ToSlash(relative))
		tagged[pkg] = append(tagged[pkg], names...)
		return nil
	})
}

// topLevelTests lists the TestXxx(t *testing.T) functions in a test file.
func topLevelTests(file string, data []byte) ([]string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), file, data, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	var names []string
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
			continue
		}
		if star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr); ok {
			if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "T" {
				names = append(names, fn.Name.Name)
			}
		}
	}
	return names, nil
}

// cloudPGProblems requires, per package, that at least one PostgreSQL-tagged
// test passed and that none was skipped: a skipped tagged test means its
// database or service was missing, which must fail the job.
func cloudPGProblems(results map[string]*packageResult, required map[string][]string) []string {
	packages := make([]string, 0, len(required))
	for pkg := range required {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	var problems []string
	for _, pkg := range packages {
		result := results[pkg]
		if result == nil {
			problems = append(problems, pkg+": did not run")
			continue
		}
		var passed int
		var skipped, missing []string
		for _, test := range required[pkg] {
			switch result.outcomes[test] {
			case "pass":
				passed++
			case "skip":
				skipped = append(skipped, test)
			case "":
				missing = append(missing, test)
			}
		}
		skipped = append(skipped, skippedPGDescendants(result, required[pkg])...)
		sort.Strings(skipped)
		switch {
		case len(skipped) > 0:
			problems = append(problems, fmt.Sprintf("%s: PostgreSQL-tagged tests skipped (%s); is a tag, GOOBERS_TEST_POSTGRES_URL or a service env var missing?",
				pkg, strings.Join(skipped, ", ")))
		case passed == 0 && result.buildFailed:
			problems = append(problems, pkg+": failed to build or set up")
		case passed == 0:
			problems = append(problems, fmt.Sprintf("%s: no PostgreSQL-tagged test passed (%d did not run)", pkg, len(missing)))
		}
	}
	return problems
}

// Only descendants of required tagged roots count; unrelated unit-test skips
// in the same package do not prove a missing PG fixture.
func skippedPGDescendants(result *packageResult, required []string) []string {
	var skipped []string
	for name, outcome := range result.outcomes {
		if outcome != "skip" {
			continue
		}
		for _, root := range required {
			if strings.HasPrefix(name, root+"/") {
				skipped = append(skipped, name)
				break
			}
		}
	}
	return skipped
}

func printFailures(results map[string]*packageResult, w io.Writer) {
	packages := make([]string, 0, len(results))
	for pkg := range results {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	for _, pkg := range packages {
		result := results[pkg]
		if len(result.failedTests) == 0 && !result.buildFailed {
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
