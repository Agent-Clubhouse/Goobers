// Command testtiming captures Go test durations and reports soft timing budgets.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const schemaVersion = 1

type artifact struct {
	SchemaVersion  int             `json:"schemaVersion"`
	Job            string          `json:"job"`
	Platform       string          `json:"platform"`
	Architecture   string          `json:"architecture"`
	ElapsedSeconds float64         `json:"elapsedSeconds"`
	Packages       []packageTiming `json:"packages"`
	Tests          []testTiming    `json:"tests,omitempty"`
}

type packageTiming struct {
	Package        string  `json:"package"`
	Status         string  `json:"status"`
	ElapsedSeconds float64 `json:"elapsedSeconds"`
}

type testTiming struct {
	Package        string  `json:"package"`
	Test           string  `json:"test"`
	Status         string  `json:"status"`
	ElapsedSeconds float64 `json:"elapsedSeconds"`
}

type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

type budgetFile struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Jobs          map[string]jobBudget `json:"jobs"`
}

type jobBudget struct {
	BaselineSeconds float64 `json:"baselineSeconds"`
	BudgetSeconds   float64 `json:"budgetSeconds"`
	Baseline        string  `json:"baseline"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now))
}

func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "capture":
		return runCapture(args[1:], stdout, stderr, now)
	case "report":
		return runReport(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "testtiming: unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(output io.Writer) {
	_, _ = fmt.Fprintln(output, "usage:")
	_, _ = fmt.Fprintln(output, "  go run ./test/testtiming capture -job JOB -out FILE -- [go test flags and packages]")
	_, _ = fmt.Fprintln(output, "  go run ./test/testtiming report -budget FILE -current FILE [-previous FILE] [-summary FILE]")
}

func runCapture(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	flags := flag.NewFlagSet("capture", flag.ContinueOnError)
	flags.SetOutput(stderr)
	job := flags.String("job", "", "stable job name")
	output := flags.String("out", "", "timing artifact path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *job == "" || *output == "" || len(flags.Args()) == 0 {
		_, _ = fmt.Fprintln(stderr, "testtiming capture: -job, -out, and go test arguments are required")
		return 2
	}

	started := now()
	result, testErr, parseErr := captureGoTest(envOrDefault("GO", "go"), flags.Args(), stdout, stderr)
	result.SchemaVersion = schemaVersion
	result.Job = *job
	result.Platform = runtime.GOOS
	result.Architecture = runtime.GOARCH
	result.ElapsedSeconds = seconds(now().Sub(started))
	if err := writeArtifact(*output, result); err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming capture: write %s: %v\n", *output, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "test timing artifact: %s\n", *output)

	if parseErr != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming capture: parse go test output: %v\n", parseErr)
		return 1
	}
	if testErr != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming capture: go test: %v\n", testErr)
		return 1
	}
	return 0
}

func captureGoTest(goCommand string, args []string, stdout, stderr io.Writer) (artifact, error, error) {
	commandArgs := append([]string{"test", "-json"}, args...)
	command := exec.Command(goCommand, commandArgs...)
	pipe, err := command.StdoutPipe()
	if err != nil {
		return artifact{}, err, nil
	}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return artifact{}, err, nil
	}
	result, parseErr := parseTestEvents(pipe, stdout)
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, pipe)
	}
	testErr := command.Wait()
	return result, testErr, parseErr
}

func parseTestEvents(input io.Reader, output io.Writer) (artifact, error) {
	packages := make(map[string]packageTiming)
	tests := make(map[string]testTiming)
	decoder := json.NewDecoder(bufio.NewReader(input))
	for {
		var event testEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return artifactFromMaps(packages, tests), err
		}
		if event.Output != "" {
			_, _ = io.WriteString(output, event.Output)
		}
		if event.Package == "" || !isTerminalAction(event.Action) {
			continue
		}
		if event.Test == "" {
			packages[event.Package] = packageTiming{
				Package:        event.Package,
				Status:         event.Action,
				ElapsedSeconds: event.Elapsed,
			}
			continue
		}
		key := event.Package + "\x00" + event.Test
		tests[key] = testTiming{
			Package:        event.Package,
			Test:           event.Test,
			Status:         event.Action,
			ElapsedSeconds: event.Elapsed,
		}
	}
	return artifactFromMaps(packages, tests), nil
}

func isTerminalAction(action string) bool {
	return action == "pass" || action == "fail" || action == "skip"
}

func artifactFromMaps(packageMap map[string]packageTiming, testMap map[string]testTiming) artifact {
	result := artifact{
		Packages: make([]packageTiming, 0, len(packageMap)),
		Tests:    make([]testTiming, 0, len(testMap)),
	}
	for _, timing := range packageMap {
		result.Packages = append(result.Packages, timing)
	}
	sort.Slice(result.Packages, func(i, j int) bool {
		return result.Packages[i].Package < result.Packages[j].Package
	})
	for _, timing := range testMap {
		result.Tests = append(result.Tests, timing)
	}
	sort.Slice(result.Tests, func(i, j int) bool {
		if result.Tests[i].Package != result.Tests[j].Package {
			return result.Tests[i].Package < result.Tests[j].Package
		}
		return result.Tests[i].Test < result.Tests[j].Test
	})
	return result
}

func writeArtifact(path string, result artifact) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func runReport(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	budgetPath := flags.String("budget", "", "timing budget path")
	currentPath := flags.String("current", "", "current timing artifact path")
	previousPath := flags.String("previous", "", "optional previous timing artifact path")
	summaryPath := flags.String("summary", os.Getenv("GITHUB_STEP_SUMMARY"), "optional GitHub step summary path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *budgetPath == "" || *currentPath == "" || len(flags.Args()) != 0 {
		_, _ = fmt.Fprintln(stderr, "testtiming report: -budget and -current are required")
		return 2
	}

	budgets, err := readBudgets(*budgetPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming report: %v\n", err)
		return 1
	}
	current, err := readArtifact(*currentPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming report: %v\n", err)
		return 1
	}
	budget, ok := budgets.Jobs[current.Job]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "testtiming report: no budget for job %q\n", current.Job)
		return 1
	}
	previous := readPrevious(*previousPath, current.Job, stderr)
	report := formatReport(current, budget, previous)
	_, _ = fmt.Fprintln(stdout, report.line)
	if report.overBudget {
		_, _ = fmt.Fprintf(stdout, "::warning title=Test timing budget exceeded::%s\n", report.line)
	}
	if *summaryPath != "" {
		if err := appendSummary(*summaryPath, report.summary); err != nil {
			_, _ = fmt.Fprintf(stderr, "testtiming report: append summary: %v\n", err)
			return 1
		}
	}
	return 0
}

func readBudgets(path string) (budgetFile, error) {
	var budgets budgetFile
	if err := readJSON(path, &budgets); err != nil {
		return budgets, fmt.Errorf("read budgets: %w", err)
	}
	if budgets.SchemaVersion != schemaVersion {
		return budgets, fmt.Errorf("budget schemaVersion = %d, want %d", budgets.SchemaVersion, schemaVersion)
	}
	if len(budgets.Jobs) == 0 {
		return budgets, errors.New("budget file has no jobs")
	}
	for name, budget := range budgets.Jobs {
		if name == "" || !validPositive(budget.BudgetSeconds) {
			return budgets, fmt.Errorf("job %q has invalid budgetSeconds", name)
		}
		if !validPositive(budget.BaselineSeconds) || budget.BaselineSeconds > budget.BudgetSeconds {
			return budgets, fmt.Errorf("job %q has invalid baselineSeconds", name)
		}
		if strings.TrimSpace(budget.Baseline) == "" {
			return budgets, fmt.Errorf("job %q has no baseline description", name)
		}
	}
	return budgets, nil
}

func readArtifact(path string) (artifact, error) {
	var result artifact
	if err := readJSON(path, &result); err != nil {
		return result, fmt.Errorf("read artifact %s: %w", path, err)
	}
	if result.SchemaVersion != schemaVersion {
		return result, fmt.Errorf("artifact %s schemaVersion = %d, want %d", path, result.SchemaVersion, schemaVersion)
	}
	if result.Job == "" || !validNonNegative(result.ElapsedSeconds) {
		return result, fmt.Errorf("artifact %s has invalid job or elapsedSeconds", path)
	}
	return result, nil
}

func readJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func readPrevious(path, job string, stderr io.Writer) *artifact {
	if path == "" {
		return nil
	}
	previous, err := readArtifact(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming report: previous timing unavailable: %v\n", err)
		return nil
	}
	if previous.Job != job {
		_, _ = fmt.Fprintf(stderr, "testtiming report: previous timing job %q does not match %q\n", previous.Job, job)
		return nil
	}
	return &previous
}

type formattedReport struct {
	line       string
	summary    string
	overBudget bool
}

func formatReport(current artifact, budget jobBudget, previous *artifact) formattedReport {
	actual := current.ElapsedSeconds
	headroom := budget.BudgetSeconds - actual
	status := "within budget"
	if headroom < 0 {
		status = "OVER BUDGET"
	}
	previousText := "not available"
	changeText := "not available"
	if previous != nil {
		previousText = durationText(previous.ElapsedSeconds)
		delta := actual - previous.ElapsedSeconds
		changeText = signedDurationText(delta)
		if previous.ElapsedSeconds > 0 {
			changeText += fmt.Sprintf(" (%+.1f%%)", delta/previous.ElapsedSeconds*100)
		}
	}

	line := fmt.Sprintf(
		"test timing %s: actual %s / budget %s (%s; headroom %s; previous %s, change %s)",
		current.Job,
		durationText(actual),
		durationText(budget.BudgetSeconds),
		status,
		signedDurationText(headroom),
		previousText,
		changeText,
	)
	summary := fmt.Sprintf(
		"## Test timing (%s/%s)\n\n| Job | Actual | Budget | Headroom | Previous | Change | Status |\n|---|---:|---:|---:|---:|---:|---|\n| `%s` | %s | %s | %s | %s | %s | **%s** |\n",
		current.Platform,
		current.Architecture,
		current.Job,
		durationText(actual),
		durationText(budget.BudgetSeconds),
		signedDurationText(headroom),
		previousText,
		changeText,
		status,
	)
	return formattedReport{line: line, summary: summary, overBudget: headroom < 0}
}

func appendSummary(path, summary string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, summary); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func durationText(value float64) string {
	return fmt.Sprintf("%.1fs", value)
}

func signedDurationText(value float64) string {
	return fmt.Sprintf("%+.1fs", value)
}

func seconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Second)
}

func validPositive(value float64) bool {
	return validNonNegative(value) && value > 0
}

func validNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
