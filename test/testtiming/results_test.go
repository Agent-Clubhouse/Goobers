package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testModule = "example.com/m"

func recordEvents(t *testing.T, events []testEvent) (artifact, *failureRecorder) {
	t.Helper()
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	recorder := newFailureRecorder()
	result, err := parseTestEvents(&input, &bytes.Buffer{}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	return result, recorder
}

// failingRun is the event stream `go test -json` emits (Go 1.24+) for one
// package with a failing subtest, a pass and a skip, plus one package whose
// test binary does not compile.
func failingRun() []testEvent {
	const pkgA, pkgB = testModule + "/a", testModule + "/b"
	build := pkgB + " [" + pkgB + ".test]"
	return []testEvent{
		{Action: "build-output", ImportPath: build, Output: "# " + build + "\n"},
		{Action: "build-output", ImportPath: build, Output: "b/b_test.go:3:27: undefined: undefined\n"},
		{Action: "build-fail", ImportPath: build},
		{Action: "start", Package: pkgB},
		{Action: "output", Package: pkgB, Output: "FAIL\t" + pkgB + " [build failed]\n"},
		{Action: "fail", Package: pkgB, FailedBuild: build},
		{Action: "output", Package: pkgA, Test: "TestOK", Output: "=== RUN   TestOK\n"},
		{Action: "pass", Package: pkgA, Test: "TestOK", Elapsed: 0.1},
		{Action: "output", Package: pkgA, Test: "TestBad", Output: "=== RUN   TestBad\n"},
		{Action: "output", Package: pkgA, Test: "TestBad/sub", Output: "=== RUN   TestBad/sub\n"},
		{Action: "output", Package: pkgA, Test: "TestBad/sub", Output: "    a_test.go:4: boom: 50%, done\n"},
		{Action: "output", Package: pkgA, Test: "TestBad/sub", Output: "        line2\n"},
		{Action: "output", Package: pkgA, Test: "TestBad/sub", Output: "--- FAIL: TestBad/sub (0.00s)\n"},
		{Action: "fail", Package: pkgA, Test: "TestBad/sub", Elapsed: 0.2},
		{Action: "output", Package: pkgA, Test: "TestBad", Output: "--- FAIL: TestBad (0.00s)\n"},
		{Action: "fail", Package: pkgA, Test: "TestBad", Elapsed: 0.2},
		{Action: "output", Package: pkgA, Test: "TestSkip", Output: "    a_test.go:5: nah\n"},
		{Action: "skip", Package: pkgA, Test: "TestSkip"},
		{Action: "output", Package: pkgA, Output: "FAIL\t" + pkgA + "\t0.5s\n"},
		{Action: "fail", Package: pkgA, Elapsed: 0.5},
	}
}

func TestAnnotationsNameTheLeafFailureAtItsSourceLine(t *testing.T) {
	t.Parallel()
	_, recorder := recordEvents(t, failingRun())
	var output bytes.Buffer
	writeAnnotations(&output, recorder.failures, testModule)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	want := []string{
		"::error file=b/b_test.go,line=3,title=build failed%3A example.com/m/b::# example.com/m/b [example.com/m/b.test]%0Ab/b_test.go:3:27: undefined: undefined%0AFAIL\texample.com/m/b [build failed]",
		"::error file=a/a_test.go,line=4,title=TestBad/sub failed (example.com/m/a)::    a_test.go:4: boom: 50%25, done%0A        line2",
	}
	if len(lines) != len(want) {
		t.Fatalf("annotations = %q, want %q", lines, want)
	}
	for index := range want {
		if lines[index] != want[index] {
			t.Fatalf("annotation %d = %q, want %q", index, lines[index], want[index])
		}
	}
}

func TestAnnotationsOmitFileOutsideTheModule(t *testing.T) {
	t.Parallel()
	_, recorder := recordEvents(t, []testEvent{
		{Action: "output", Package: "other.org/x", Test: "TestX", Output: "    x_test.go:9: nope\n"},
		{Action: "fail", Package: "other.org/x", Test: "TestX"},
		{Action: "fail", Package: "other.org/x"},
	})
	var output bytes.Buffer
	writeAnnotations(&output, recorder.failures, testModule)
	if got := output.String(); got != "::error title=TestX failed (other.org/x)::    x_test.go:9: nope\n" {
		t.Fatalf("annotation = %q", got)
	}
}

func TestAnnotationsReportAPackageFailureWithoutAFailingTest(t *testing.T) {
	t.Parallel()
	_, recorder := recordEvents(t, []testEvent{
		{Action: "output", Package: testModule + "/c", Output: "panic: boom in TestMain\n"},
		{Action: "fail", Package: testModule + "/c"},
	})
	var output bytes.Buffer
	writeAnnotations(&output, recorder.failures, testModule)
	if got := output.String(); got != "::error title=package failed%3A example.com/m/c::panic: boom in TestMain\n" {
		t.Fatalf("annotation = %q", got)
	}
}

func TestAnnotationsStopAtTheRunnerLimit(t *testing.T) {
	t.Parallel()
	var events []testEvent
	for index := range maxAnnotations + 3 {
		name := "Test" + strings.Repeat("X", index+1)
		events = append(events, testEvent{Action: "fail", Package: testModule, Test: name})
	}
	_, recorder := recordEvents(t, events)
	var output bytes.Buffer
	writeAnnotations(&output, recorder.failures, testModule)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != maxAnnotations+1 {
		t.Fatalf("got %d annotation lines, want %d: %q", len(lines), maxAnnotations+1, lines)
	}
	if last := lines[maxAnnotations]; !strings.HasPrefix(last, "::notice ") || !strings.Contains(last, "3 more failing tests") {
		t.Fatalf("overflow line = %q", last)
	}
}

func TestPassingRunAnnotatesNothing(t *testing.T) {
	t.Parallel()
	_, recorder := recordEvents(t, []testEvent{
		{Action: "output", Package: testModule, Test: "TestOK", Output: "    ok_test.go:1: log line\n"},
		{Action: "pass", Package: testModule, Test: "TestOK"},
		{Action: "pass", Package: testModule},
	})
	var output bytes.Buffer
	writeAnnotations(&output, recorder.failures, testModule)
	if output.Len() != 0 || len(recorder.running) != 0 {
		t.Fatalf("annotations = %q, retained output = %d", output.String(), len(recorder.running))
	}
}

func TestRecordedOutputKeepsTheTail(t *testing.T) {
	t.Parallel()
	record := &outputRecord{}
	record.append("    first_test.go:7: first\n")
	record.append(strings.Repeat("x", maxRecordedOutput))
	record.append("verdict\n")
	got := record.text()
	if !strings.HasSuffix(got, "verdict\n") || !strings.HasPrefix(got, "[... ") {
		t.Fatalf("recorded output head = %q, tail = %q", got[:40], got[len(got)-20:])
	}
	if record.file != "first_test.go" || record.line != "7" {
		t.Fatalf("location = %s:%s, want the first reported line", record.file, record.line)
	}
}

func TestJUnitReportListsEveryCaseWithFailureOutput(t *testing.T) {
	t.Parallel()
	result, recorder := recordEvents(t, failingRun())
	path := filepath.Join(t.TempDir(), "results", "unit.junit.xml")
	var stdout, stderr bytes.Buffer
	if !writeResults(path, false, result, recorder.failures, &stdout, &stderr) {
		t.Fatalf("writeResults failed: %s", stderr.String())
	}
	if strings.Contains(stdout.String(), "::error") {
		t.Fatalf("annotations printed with annotate=false: %q", stdout.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report junitTestSuites
	if err := xml.Unmarshal(data, &report); err != nil {
		t.Fatalf("report is not valid XML: %v\n%s", err, data)
	}
	if report.Tests != 5 || report.Failures != 3 || report.Skipped != 1 || len(report.Suites) != 2 {
		t.Fatalf("totals = %d tests, %d failures, %d skipped, %d suites", report.Tests, report.Failures, report.Skipped, len(report.Suites))
	}
	cases := map[string]junitTestCase{}
	for _, suite := range report.Suites {
		for _, current := range suite.Cases {
			cases[current.Classname+" "+current.Name] = current
		}
	}
	sub := cases[testModule+"/a TestBad/sub"]
	if sub.Failure == nil || !strings.Contains(sub.Failure.Output, "a_test.go:4: boom: 50%, done\n        line2") {
		t.Fatalf("TestBad/sub = %#v", sub)
	}
	if skipped := cases[testModule+"/a TestSkip"]; skipped.Skipped == nil || skipped.Failure != nil {
		t.Fatalf("TestSkip = %#v", skipped)
	}
	if passed := cases[testModule+"/a TestOK"]; passed.Failure != nil || passed.Skipped != nil || passed.Time != "0.100" {
		t.Fatalf("TestOK = %#v", passed)
	}
	build := cases[testModule+"/b "+packageFailureCase]
	if build.Failure == nil || !strings.Contains(build.Failure.Output, "undefined: undefined") {
		t.Fatalf("build failure case = %#v", build)
	}
}

func TestReadModulePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := readModulePath(dir); got != "" {
		t.Fatalf("missing go.mod module = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("// comment\nmodule example.com/m\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readModulePath(dir); got != testModule {
		t.Fatalf("module = %q", got)
	}
}
