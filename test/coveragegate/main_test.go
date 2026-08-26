package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Mock implementations for testing

type mockFS struct {
	mu         sync.Mutex
	statErr    error
	readErr    error
	createErr  error
	removeErr  error
	readData   []byte
	statPath   string
	readPath   string
	createPath string
	removePath string
	tempFile   tempFile
}

func (m *mockFS) stat(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statPath = path
	return m.statErr
}

func (m *mockFS) readFile(path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readPath = path
	if m.readErr != nil {
		return nil, m.readErr
	}
	if m.readData != nil {
		return m.readData, nil
	}
	return []byte(validProfile), nil
}

func (m *mockFS) createTemp(dir, pattern string) (tempFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createPath = dir
	if m.createErr != nil {
		return nil, m.createErr
	}
	return m.tempFile, nil
}

func (m *mockFS) remove(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removePath = path
	return m.removeErr
}

type mockTempFile struct {
	mu          sync.Mutex
	writeErr    error
	closeErr    error
	closeCalled bool
	writtenData []byte
	fileName    string
}

func (m *mockTempFile) write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writtenData = b
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return len(b), nil
}

func (m *mockTempFile) close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalled = true
	return m.closeErr
}

func (m *mockTempFile) name() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fileName
}

type mockExec struct {
	mu             sync.Mutex
	generateErr    error
	functionErr    error
	functionOutput []byte
	generateCalled bool
	functionCalled bool
}

type directoryExec struct {
	dir string
}

func (e *directoryExec) generateProfile(string, io.Writer, io.Writer) error {
	return errors.New("profile generation is not expected")
}

func (e *directoryExec) functionCoverage(profile string) ([]byte, error) {
	cmd := exec.Command("go", "tool", "cover", "-func="+profile)
	cmd.Dir = e.dir
	return cmd.CombinedOutput()
}

func (m *mockExec) generateProfile(profile string, stdout, stderr io.Writer) error {
	m.mu.Lock()
	m.generateCalled = true
	generateErr := m.generateErr
	m.mu.Unlock()

	if generateErr != nil {
		return generateErr
	}
	_, _ = io.WriteString(stdout, "ok\n")
	return nil
}

func (m *mockExec) functionCoverage(profile string) ([]byte, error) {
	m.mu.Lock()
	m.functionCalled = true
	functionErr := m.functionErr
	functionOutput := m.functionOutput
	m.mu.Unlock()

	if functionErr != nil {
		return nil, functionErr
	}
	return functionOutput, nil
}

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

func TestLinuxOnlySourceEntersProfileAndCanFailGate(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": `module example.com/platformcoverage

go 1.26
`,
		"linux_only.go": `//go:build linux || coverage_platform

package platformcoverage

func linuxOnly() int {
	return 42
}

func covered() {
	_ = 1
}
`,
		"linux_only_test.go": `package platformcoverage

import "testing"

func TestCovered(t *testing.T) {
	covered()
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	profile := filepath.Join(dir, "coverage.out")
	cmd := exec.Command("go", "test", "-tags=coverage_platform", "-covermode=set", "-coverprofile="+profile, "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate platform coverage profile: %v\n%s", err, output)
	}

	raw, err := os.ReadFile(profile)
	if err != nil {
		t.Fatalf("read generated profile: %v", err)
	}
	if !strings.Contains(string(raw), "linux_only.go") {
		t.Fatalf("generated Linux coverage profile omits platform-specific source:\n%s", raw)
	}

	t.Setenv("COVERAGE_PROFILE", profile)
	t.Setenv("COVERAGE_EXCLUDE", "$^")
	var stdout, stderr bytes.Buffer
	if code := runWithProviders([]string{"100"}, &stdout, &stderr, osFS{}, &directoryExec{dir: dir}); code != 1 {
		t.Fatalf("uncovered platform-specific source must fail a 100%% gate: exit=%d\nstdout:\n%s\nstderr:\n%s", code, &stdout, &stderr)
	}
	if !strings.Contains(stdout.String(), "linuxOnly") ||
		!strings.Contains(stderr.String(), "FAIL: coverage") {
		t.Fatalf("gate output does not expose the uncovered platform-specific function:\nstdout:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
}

// Operational failure-path tests

const validProfile = `mode: atomic
github.com/goobers/goobers/internal/runner/run.go:10.1,12.2 2 1
github.com/goobers/goobers/internal/runner/run.go:14.1,16.2 2 0
`

const validCoverageReport = `github.com/goobers/goobers/internal/runner/run.go:10.1,12.2	covered	Run	1	1
github.com/goobers/goobers/internal/runner/run.go:14.1,16.2	uncovered	covered	0	1
total:		(statements)	50.0%
`

func TestProfileGenerationError(t *testing.T) {
	t.Parallel()
	mockFS := &mockFS{
		statErr:  os.ErrNotExist,
		tempFile: &mockTempFile{fileName: "temp.out"},
	}
	mockExec := &mockExec{
		generateErr: errors.New("go test failed"),
	}

	var stdout, stderr bytes.Buffer
	code := runWithProviders([]string{"70"}, &stdout, &stderr, mockFS, mockExec)

	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "generate profile") {
		t.Fatalf("expected error about generating profile, got: %s", stderr.String())
	}

	mockExec.mu.Lock()
	generateCalled := mockExec.generateCalled
	mockExec.mu.Unlock()

	if !generateCalled {
		t.Fatal("generateProfile was not called")
	}
}

func TestProfileStatError(t *testing.T) {
	t.Parallel()
	mockFS := &mockFS{
		statErr: errors.New("permission denied"),
	}
	mockExec := &mockExec{}

	var stdout, stderr bytes.Buffer
	code := runWithProviders([]string{"70"}, &stdout, &stderr, mockFS, mockExec)

	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "inspect") {
		t.Fatalf("expected error about inspecting profile, got: %s", stderr.String())
	}
}

func TestProfileReadError(t *testing.T) {
	t.Parallel()
	mockFS := &mockFS{
		statErr: nil,
		readErr: errors.New("read error"),
	}
	mockExec := &mockExec{}

	var stdout, stderr bytes.Buffer
	code := runWithProviders([]string{"70"}, &stdout, &stderr, mockFS, mockExec)

	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "read") {
		t.Fatalf("expected error about reading profile, got: %s", stderr.String())
	}
}

func TestInvalidExclusionRegex(t *testing.T) {
	t.Setenv("COVERAGE_EXCLUDE", "[invalid(regex")

	var stdout, stderr bytes.Buffer
	code := run([]string{"70"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "invalid exclusion regex") {
		t.Fatalf("expected error about invalid regex, got: %s", stderr.String())
	}
}

func TestTempFileCreateError(t *testing.T) {
	t.Parallel()
	mockFS := &mockFS{
		statErr:   nil,
		readErr:   nil,
		createErr: errors.New("cannot create temp file"),
	}
	mockExec := &mockExec{}

	var stdout, stderr bytes.Buffer
	code := runWithProviders([]string{"70"}, &stdout, &stderr, mockFS, mockExec)

	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "create filtered profile") {
		t.Fatalf("expected error about creating filtered profile, got: %s", stderr.String())
	}
}

func TestTempFileWriteError(t *testing.T) {
	t.Parallel()
	mockTempFile := &mockTempFile{
		fileName: "temp.out",
		writeErr: errors.New("write failed"),
	}
	mockFS := &mockFS{
		statErr:  nil,
		readErr:  nil,
		tempFile: mockTempFile,
	}
	mockExec := &mockExec{}

	var stdout, stderr bytes.Buffer
	code := runWithProviders([]string{"70"}, &stdout, &stderr, mockFS, mockExec)

	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "write filtered profile") {
		t.Fatalf("expected error about writing filtered profile, got: %s", stderr.String())
	}

	mockTempFile.mu.Lock()
	closeCalled := mockTempFile.closeCalled
	mockTempFile.mu.Unlock()

	if !closeCalled {
		t.Fatal("temp file close was not called after write error")
	}
}

func TestTempFileCloseError(t *testing.T) {
	t.Parallel()
	mockTempFile := &mockTempFile{
		fileName: "temp.out",
		closeErr: errors.New("close failed"),
	}
	mockFS := &mockFS{
		statErr:  nil,
		readErr:  nil,
		tempFile: mockTempFile,
	}
	mockExec := &mockExec{}

	var stdout, stderr bytes.Buffer
	code := runWithProviders([]string{"70"}, &stdout, &stderr, mockFS, mockExec)

	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "close filtered profile") {
		t.Fatalf("expected error about closing filtered profile, got: %s", stderr.String())
	}
}

func TestFunctionCoverageError(t *testing.T) {
	t.Parallel()
	mockTempFile := &mockTempFile{fileName: "temp.out"}
	mockFS := &mockFS{
		statErr:  nil,
		readErr:  nil,
		tempFile: mockTempFile,
	}
	mockExec := &mockExec{
		functionErr: errors.New("go tool cover failed: invalid profile"),
	}

	var stdout, stderr bytes.Buffer
	code := runWithProviders([]string{"70"}, &stdout, &stderr, mockFS, mockExec)

	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "calculate coverage") {
		t.Fatalf("expected error about calculating coverage, got: %s", stderr.String())
	}
}

func TestEndToEndWithMocks(t *testing.T) {
	t.Parallel()
	mockTempFile := &mockTempFile{fileName: "temp.out"}
	mockFS := &mockFS{
		statErr:  nil,
		tempFile: mockTempFile,
	}
	mockExec := &mockExec{
		functionOutput: []byte(validCoverageReport),
	}

	var stdout, stderr bytes.Buffer
	code := runWithProviders([]string{"50"}, &stdout, &stderr, mockFS, mockExec)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "PASS: coverage gate satisfied") {
		t.Fatalf("expected pass message in stdout, got: %s", stdout.String())
	}

	mockExec.mu.Lock()
	functionCalled := mockExec.functionCalled
	mockExec.mu.Unlock()

	if !functionCalled {
		t.Fatal("functionCoverage was not called")
	}
}

func TestGateClosed_BelowThreshold(t *testing.T) {
	t.Parallel()
	mockTempFile := &mockTempFile{fileName: "temp.out"}
	mockFS := &mockFS{
		statErr:  nil,
		tempFile: mockTempFile,
	}
	mockExec := &mockExec{
		functionOutput: []byte(validCoverageReport),
	}

	var stdout, stderr bytes.Buffer
	code := runWithProviders([]string{"51"}, &stdout, &stderr, mockFS, mockExec)

	if code != 1 {
		t.Fatalf("expected exit code 1 (gate fail), got %d", code)
	}
	if !strings.Contains(stderr.String(), "FAIL: coverage 50.0% is below threshold 51%") {
		t.Fatalf("expected fail message in stderr, got: %s", stderr.String())
	}
}
