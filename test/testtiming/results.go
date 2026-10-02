package main

// Structured test results for CI consumers (#681): a JUnit XML report of every
// test the capture observed, and GitHub workflow annotations for the failing
// ones, so a red run's summary page names what failed without anyone reading
// the full job log. Both are derived from the same `go test -json` stream the
// timing capture already parses; neither changes the capture's exit status.

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// maxRecordedOutput bounds the output kept per test or package. The tail
	// is kept: a failing test's verdict and any panic trace sit at the end.
	maxRecordedOutput = 64 << 10
	// maxAnnotations matches GitHub's per-step display limit for error
	// annotations; anything past it would be silently dropped by the runner.
	maxAnnotations = 10
	// maxAnnotationLines keeps one annotation readable on the summary page;
	// the full output is in the JUnit report. It is long enough to carry a
	// test panic through its first non-runtime stack frame, which is what
	// test/flakewatch fingerprints when it reads these annotations.
	maxAnnotationLines = 50
	// packageFailureCase names the synthetic JUnit case for a package that
	// failed without a failing test (build failure, panic in TestMain, a
	// timeout outside any test).
	packageFailureCase = "[package]"
)

var (
	// testLocation matches the "    file_test.go:42: message" lines testing.T
	// prefixes to Error/Fatal output; the file is relative to the package.
	testLocation = regexp.MustCompile(`^\s+([^\s:/\\]+\.go):(\d+): `)
	// buildLocation matches compiler diagnostics, already relative to the
	// directory go test ran in (the module root).
	buildLocation = regexp.MustCompile(`^([^\s:]+\.go):(\d+):(?:\d+:)? `)
)

// failure is one failed test, or a package that failed on its own.
type failure struct {
	Package string
	Test    string
	Output  string
	File    string // relative to the package directory (tests) or module root (builds)
	Line    string
	build   bool
}

type outputRecord struct {
	output    strings.Builder
	truncated int
	file      string
	line      string
}

func (r *outputRecord) append(text string) {
	if r.file == "" {
		if match := testLocation.FindStringSubmatch(text); match != nil {
			r.file, r.line = match[1], match[2]
		}
	}
	r.output.WriteString(text)
	if excess := r.output.Len() - maxRecordedOutput; excess > 0 {
		kept := r.output.String()[excess:]
		r.truncated += excess
		r.output.Reset()
		r.output.WriteString(kept)
	}
}

func (r *outputRecord) text() string {
	if r.truncated == 0 {
		return r.output.String()
	}
	return fmt.Sprintf("[... %d bytes of earlier output truncated ...]\n%s", r.truncated, r.output.String())
}

// failureRecorder keeps the output of every test and package still running,
// and retains it only for those that fail.
type failureRecorder struct {
	running  map[string]*outputRecord
	builds   map[string]*outputRecord
	failures []failure
}

func newFailureRecorder() *failureRecorder {
	return &failureRecorder{running: make(map[string]*outputRecord), builds: make(map[string]*outputRecord)}
}

func (r *failureRecorder) observe(event testEvent) {
	if r == nil {
		return
	}
	if event.Action == "build-output" && event.ImportPath != "" {
		record := r.builds[event.ImportPath]
		if record == nil {
			record = &outputRecord{}
			r.builds[event.ImportPath] = record
		}
		record.append(event.Output)
		if record.file == "" {
			if match := buildLocation.FindStringSubmatch(event.Output); match != nil {
				record.file, record.line = match[1], match[2]
			}
		}
		return
	}
	if event.Package == "" {
		return
	}
	key := event.Package + "\x00" + event.Test
	if event.Output != "" {
		record := r.running[key]
		if record == nil {
			record = &outputRecord{}
			r.running[key] = record
		}
		record.append(event.Output)
	}
	if !isTerminalAction(event.Action) {
		return
	}
	record := r.running[key]
	delete(r.running, key)
	if event.Action != "fail" {
		return
	}
	if record == nil {
		record = &outputRecord{}
	}
	recorded := failure{Package: event.Package, Test: event.Test, Output: record.text(), File: record.file, Line: record.line}
	if build := r.builds[event.FailedBuild]; event.FailedBuild != "" && build != nil {
		recorded.Output = build.text() + recorded.Output
		recorded.File, recorded.Line, recorded.build = build.file, build.line, true
	}
	r.failures = append(r.failures, recorded)
}

// leafFailures drops a failed test whose failure is explained by a failed
// subtest, and a failed package whose failure is explained by a failed test,
// so each annotation names the most specific thing that failed.
func leafFailures(failures []failure) []failure {
	result := make([]failure, 0, len(failures))
	for _, candidate := range failures {
		explained := false
		for _, other := range failures {
			if other.Package != candidate.Package || other.Test == candidate.Test || other.Test == "" {
				continue
			}
			if candidate.Test == "" || strings.HasPrefix(other.Test, candidate.Test+"/") {
				explained = true
				break
			}
		}
		if !explained {
			result = append(result, candidate)
		}
	}
	return result
}

// writeAnnotations prints GitHub workflow error annotations for the leaf
// failures, up to the runner's display limit, then a notice for the rest.
func writeAnnotations(output io.Writer, failures []failure, modulePath string) {
	leaves := leafFailures(failures)
	for index, current := range leaves {
		if index == maxAnnotations {
			_, _ = fmt.Fprintf(output, "::notice title=%s::%s\n",
				escapeAnnotationProperty("More failing tests"),
				escapeAnnotationData(fmt.Sprintf("%d more failing tests or packages are not annotated; see the JUnit test-results artifact.", len(leaves)-maxAnnotations)))
			return
		}
		properties := []string{}
		if file := annotationFile(current, modulePath); file != "" {
			properties = append(properties, "file="+escapeAnnotationProperty(file))
			if current.Line != "" {
				properties = append(properties, "line="+escapeAnnotationProperty(current.Line))
			}
		}
		properties = append(properties, "title="+escapeAnnotationProperty(annotationTitle(current)))
		_, _ = fmt.Fprintf(output, "::error %s::%s\n", strings.Join(properties, ","), escapeAnnotationData(annotationMessage(current.Output)))
	}
}

func annotationTitle(current failure) string {
	if current.Test == "" {
		if current.build {
			return "build failed: " + current.Package
		}
		return "package failed: " + current.Package
	}
	return current.Test + " failed (" + current.Package + ")"
}

// annotationFile maps a failure location to a repository-relative path, or
// "" when it cannot be derived (a package outside the module, no location).
func annotationFile(current failure, modulePath string) string {
	if current.File == "" {
		return ""
	}
	if current.build {
		if filepath.IsAbs(current.File) {
			return ""
		}
		return path.Clean(filepath.ToSlash(current.File))
	}
	if modulePath == "" {
		return ""
	}
	if current.Package == modulePath {
		return current.File
	}
	directory, ok := strings.CutPrefix(current.Package, modulePath+"/")
	if !ok {
		return ""
	}
	return path.Join(directory, current.File)
}

// annotationMessage drops the framing lines go test adds around every test
// and keeps the first lines of what the test actually reported.
func annotationMessage(output string) string {
	var kept []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "=== ") || strings.HasPrefix(trimmed, "--- FAIL") {
			continue
		}
		kept = append(kept, strings.TrimRight(line, " \t\r"))
		if len(kept) == maxAnnotationLines {
			kept = append(kept, "[... see the JUnit test-results artifact for the full output ...]")
			break
		}
	}
	if len(kept) == 0 {
		return "failed with no output"
	}
	return strings.Join(kept, "\n")
}

func escapeAnnotationData(value string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(value)
}

func escapeAnnotationProperty(value string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(value)
}

// readModulePath returns the module path declared by dir/go.mod, or "" when
// it cannot be read; annotations then omit file locations for tests.
func readModulePath(dir string) string {
	file, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

type junitTestSuites struct {
	XMLName  xml.Name         `xml:"testsuites"`
	Tests    int              `xml:"tests,attr"`
	Failures int              `xml:"failures,attr"`
	Skipped  int              `xml:"skipped,attr"`
	Time     string           `xml:"time,attr"`
	Suites   []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	Name     string          `xml:"name,attr"`
	Tests    int             `xml:"tests,attr"`
	Failures int             `xml:"failures,attr"`
	Skipped  int             `xml:"skipped,attr"`
	Time     string          `xml:"time,attr"`
	Cases    []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	Classname string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Output  string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

// buildJUnit renders one testsuite per package and one testcase per test
// (subtests included), attaching recorded output to failed cases. A package
// that failed with no failing test gets a synthetic case so the failure is
// never invisible to a JUnit consumer.
func buildJUnit(result artifact, failures []failure) junitTestSuites {
	failed := make(map[string]failure, len(failures))
	for _, current := range failures {
		failed[current.Package+"\x00"+current.Test] = current
	}
	suites := make(map[string]*junitTestSuite)
	suite := func(pkg string) *junitTestSuite {
		if existing := suites[pkg]; existing != nil {
			return existing
		}
		created := &junitTestSuite{Name: pkg, Time: secondsText(0)}
		suites[pkg] = created
		return created
	}
	for _, timing := range result.Packages {
		suite(timing.Package).Time = secondsText(timing.ElapsedSeconds)
	}
	for _, timing := range result.Tests {
		current := junitTestCase{Classname: timing.Package, Name: timing.Test, Time: secondsText(timing.ElapsedSeconds)}
		target := suite(timing.Package)
		switch timing.Status {
		case "fail":
			current.Failure = &junitFailure{Message: "Failed", Output: failed[timing.Package+"\x00"+timing.Test].Output}
			target.Failures++
		case "skip":
			current.Skipped = &junitSkipped{Message: "Skipped"}
			target.Skipped++
		}
		target.Tests++
		target.Cases = append(target.Cases, current)
	}
	for _, current := range leafFailures(failures) {
		if current.Test != "" {
			continue
		}
		target := suite(current.Package)
		target.Cases = append(target.Cases, junitTestCase{
			Classname: current.Package,
			Name:      packageFailureCase,
			Time:      target.Time,
			Failure:   &junitFailure{Message: annotationTitle(current), Output: current.Output},
		})
		target.Tests++
		target.Failures++
	}

	names := make([]string, 0, len(suites))
	for name := range suites {
		names = append(names, name)
	}
	sort.Strings(names)
	report := junitTestSuites{Time: secondsText(result.ElapsedSeconds)}
	for _, name := range names {
		current := *suites[name]
		report.Tests += current.Tests
		report.Failures += current.Failures
		report.Skipped += current.Skipped
		report.Suites = append(report.Suites, current)
	}
	return report
}

func writeJUnit(path string, report junitTestSuites) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, xml.Header); err != nil {
		_ = file.Close()
		return err
	}
	encoder := xml.NewEncoder(file)
	encoder.Indent("", "  ")
	if err := encoder.Encode(report); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := io.WriteString(file, "\n"); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func secondsText(value float64) string {
	return fmt.Sprintf("%.3f", value)
}

// writeResults writes the JUnit report and prints annotations. A failure to
// write the report fails the capture like a failure to write the timing
// artifact; annotations are best-effort output.
func writeResults(junitPath string, annotate bool, result artifact, failures []failure, stdout, stderr io.Writer) bool {
	if junitPath != "" {
		if err := writeJUnit(junitPath, buildJUnit(result, failures)); err != nil {
			_, _ = fmt.Fprintf(stderr, "testtiming capture: write %s: %v\n", junitPath, err)
			return false
		}
		_, _ = fmt.Fprintf(stdout, "test results (JUnit): %s\n", junitPath)
	}
	if annotate {
		writeAnnotations(stdout, failures, readModulePath("."))
	}
	return true
}
