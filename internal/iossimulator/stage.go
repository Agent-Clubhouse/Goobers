// Package iossimulator drives one XCUITest invocation against an installed
// iPhone simulator and reduces its xcresult bundle to workflow-safe scalars.
package iossimulator

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	// DefaultResultBundle is the xcresult bundle path used when none is specified.
	DefaultResultBundle = "ios-simulator.xcresult"
	maxDiagnosticLength = 2048
)

// Options configures an XCUITest invocation and simulator selection.
type Options struct {
	Project      string
	Workspace    string
	Scheme       string
	Device       string
	Runtime      string
	OnlyTesting  string
	ResultBundle string
}

// Result is the workflow-safe outcome parsed from an XCUITest result bundle.
type Result struct {
	Passed            bool   `json:"passed"`
	Outcome           string `json:"outcome"`
	XcodeVersion      string `json:"xcodeVersion,omitempty"`
	SimulatorName     string `json:"simulatorName,omitempty"`
	SimulatorUDID     string `json:"simulatorUDID,omitempty"`
	SimulatorRuntime  string `json:"simulatorRuntime,omitempty"`
	ResultBundlePath  string `json:"resultBundlePath,omitempty"`
	TotalTests        int    `json:"totalTests"`
	PassedTests       int    `json:"passedTests"`
	FailedTests       int    `json:"failedTests"`
	SkippedTests      int    `json:"skippedTests"`
	DiagnosticSummary string `json:"diagnosticSummary,omitempty"`
	ErrorCode         string `json:"errorCode,omitempty"`
	ErrorMessage      string `json:"errorMessage,omitempty"`
	ErrorRetryable    bool   `json:"errorRetryable,omitempty"`
}

// Report contains the parsed result and raw command output used for diagnostics.
type Report struct {
	Result        Result
	XcodeOutput   []byte
	SummaryOutput []byte
}

// ToolRunner runs the Xcode command-line tools used by the stage.
type ToolRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// ExecToolRunner runs tools as local subprocesses.
type ExecToolRunner struct{}

// Run executes a tool and returns its combined output.
func (ExecToolRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type runtimeList struct {
	Runtimes []simulatorRuntime `json:"runtimes"`
}

type simulatorRuntime struct {
	Identifier   string `json:"identifier"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	BuildVersion string `json:"buildversion"`
	IsAvailable  bool   `json:"isAvailable"`
}

type deviceList struct {
	Devices map[string][]simulatorDevice `json:"devices"`
}

type simulatorDevice struct {
	UDID                 string `json:"udid"`
	Name                 string `json:"name"`
	State                string `json:"state"`
	DeviceTypeIdentifier string `json:"deviceTypeIdentifier"`
	IsAvailable          bool   `json:"isAvailable"`
}

type resultSummary struct {
	Result       string        `json:"result"`
	TotalTests   int           `json:"totalTestCount"`
	PassedTests  int           `json:"passedTests"`
	FailedTests  int           `json:"failedTests"`
	SkippedTests int           `json:"skippedTests"`
	TestFailures []testFailure `json:"testFailures"`
}

type testFailure struct {
	TestName             string `json:"testName"`
	TargetName           string `json:"targetName"`
	FailureText          string `json:"failureText"`
	TestIdentifierString string `json:"testIdentifierString"`
}

// ValidateHost rejects hosts that cannot run iOS Simulator stages.
func ValidateHost(goos string) error {
	if goos != "darwin" {
		return fmt.Errorf("iOS Simulator stages require macOS (host OS is %s)", goos)
	}
	return nil
}

// ValidateOptions verifies that options identify one Xcode container and a safe result path.
func ValidateOptions(options Options) error {
	if (options.Project == "") == (options.Workspace == "") {
		return fmt.Errorf("exactly one of project or workspace is required")
	}
	if strings.TrimSpace(options.Scheme) == "" {
		return fmt.Errorf("scheme is required")
	}
	bundle := options.ResultBundle
	if bundle == "" {
		bundle = DefaultResultBundle
	}
	if err := validateRelativePath(bundle); err != nil {
		return fmt.Errorf("result bundle: %w", err)
	}
	return nil
}

// Run executes XCUITest against a selected simulator and parses its result bundle.
func Run(ctx context.Context, options Options, tools ToolRunner) Report {
	result := Result{Outcome: "error"}
	if err := ValidateOptions(options); err != nil {
		return failedReport(result, "invalid_ios_simulator_options", err.Error())
	}
	if tools == nil {
		return failedReport(result, "ios_simulator_tooling_error", "tool runner is required")
	}
	if options.ResultBundle == "" {
		options.ResultBundle = DefaultResultBundle
	}
	result.ResultBundlePath = filepath.ToSlash(options.ResultBundle)

	versionOutput, err := tools.Run(ctx, "xcodebuild", "-version")
	if err != nil {
		return failedReport(result, "xcode_unavailable", commandError("xcodebuild -version", versionOutput, err))
	}
	result.XcodeVersion = formatXcodeVersion(versionOutput)

	runtimesOutput, err := tools.Run(ctx, "xcrun", "simctl", "list", "runtimes", "available", "--json")
	if err != nil {
		return failedReport(result, "simulator_discovery_failed", commandError("xcrun simctl list runtimes", runtimesOutput, err))
	}
	var runtimes runtimeList
	if err := json.Unmarshal(runtimesOutput, &runtimes); err != nil {
		return failedReport(result, "simulator_discovery_failed", fmt.Sprintf("decode simulator runtimes: %v", err))
	}

	devicesOutput, err := tools.Run(ctx, "xcrun", "simctl", "list", "devices", "available", "--json")
	if err != nil {
		return failedReport(result, "simulator_discovery_failed", commandError("xcrun simctl list devices", devicesOutput, err))
	}
	var devices deviceList
	if err := json.Unmarshal(devicesOutput, &devices); err != nil {
		return failedReport(result, "simulator_discovery_failed", fmt.Sprintf("decode simulator devices: %v", err))
	}

	selectedRuntime, selectedDevice, err := selectSimulator(runtimes.Runtimes, devices.Devices, options.Runtime, options.Device)
	if err != nil {
		return failedReport(result, "simulator_not_found", err.Error())
	}
	result.SimulatorName = selectedDevice.Name
	result.SimulatorUDID = selectedDevice.UDID
	result.SimulatorRuntime = formatRuntime(selectedRuntime)

	if !strings.EqualFold(selectedDevice.State, "Booted") {
		output, bootErr := tools.Run(ctx, "xcrun", "simctl", "boot", selectedDevice.UDID)
		if bootErr != nil {
			return failedReport(result, "simulator_boot_failed", commandError("xcrun simctl boot", output, bootErr))
		}
	}
	output, err := tools.Run(ctx, "xcrun", "simctl", "bootstatus", selectedDevice.UDID, "-b")
	if err != nil {
		return failedReport(result, "simulator_boot_failed", commandError("xcrun simctl bootstatus", output, err))
	}

	xcodeArgs := []string{"test"}
	if options.Project != "" {
		xcodeArgs = append(xcodeArgs, "-project", options.Project)
	} else {
		xcodeArgs = append(xcodeArgs, "-workspace", options.Workspace)
	}
	xcodeArgs = append(xcodeArgs,
		"-scheme", options.Scheme,
		"-destination", "platform=iOS Simulator,id="+selectedDevice.UDID,
		"-resultBundlePath", options.ResultBundle,
		"-parallel-testing-enabled", "NO",
	)
	if options.OnlyTesting != "" {
		xcodeArgs = append(xcodeArgs, "-only-testing:"+options.OnlyTesting)
	}
	xcodeOutput, xcodeErr := tools.Run(ctx, "xcodebuild", xcodeArgs...)

	summaryOutput, summaryErr := tools.Run(ctx, "xcrun", "xcresulttool", "get", "test-results", "summary", "--path", options.ResultBundle, "--compact")
	report := Report{Result: result, XcodeOutput: xcodeOutput, SummaryOutput: summaryOutput}
	if summaryErr != nil {
		message := commandError("xcrun xcresulttool get test-results summary", summaryOutput, summaryErr)
		if xcodeErr != nil {
			message = commandError("xcodebuild test", xcodeOutput, xcodeErr) + "; " + message
		}
		report.Result = failedResult(result, "xcresult_parse_failed", message)
		return report
	}

	var summary resultSummary
	if err := json.Unmarshal(summaryOutput, &summary); err != nil {
		report.Result = failedResult(result, "xcresult_parse_failed", fmt.Sprintf("decode xcresult summary: %v", err))
		return report
	}
	report.Result.Outcome = summary.Result
	report.Result.TotalTests = summary.TotalTests
	report.Result.PassedTests = summary.PassedTests
	report.Result.FailedTests = summary.FailedTests
	report.Result.SkippedTests = summary.SkippedTests

	if summary.TotalTests == 0 {
		report.Result = failedResult(report.Result, "ios_tests_not_found", "xcresult reported zero executed tests")
		return report
	}
	if xcodeErr != nil || !strings.EqualFold(summary.Result, "Passed") || summary.FailedTests > 0 {
		message := failureSummary(summary, xcodeOutput, xcodeErr)
		report.Result = failedResult(report.Result, "ios_tests_failed", message)
		return report
	}
	report.Result.Passed = true
	report.Result.DiagnosticSummary = fmt.Sprintf("%d tests passed on %s with %s", summary.PassedTests, result.SimulatorRuntime, result.XcodeVersion)
	return report
}

func selectSimulator(runtimes []simulatorRuntime, devices map[string][]simulatorDevice, runtimeFilter, deviceName string) (simulatorRuntime, simulatorDevice, error) {
	candidates := make([]simulatorRuntime, 0, len(runtimes))
	for _, candidate := range runtimes {
		if !candidate.IsAvailable || !strings.Contains(candidate.Identifier, ".SimRuntime.iOS-") {
			continue
		}
		if runtimeFilter != "" &&
			!strings.EqualFold(candidate.Identifier, runtimeFilter) &&
			!strings.EqualFold(candidate.Name, runtimeFilter) &&
			!strings.EqualFold(candidate.Version, runtimeFilter) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return compareVersions(candidates[i].Version, candidates[j].Version) > 0
	})

	for _, candidate := range candidates {
		matches := make([]simulatorDevice, 0, len(devices[candidate.Identifier]))
		for _, device := range devices[candidate.Identifier] {
			if !device.IsAvailable || !strings.Contains(device.DeviceTypeIdentifier, ".iPhone") {
				continue
			}
			if deviceName != "" && !strings.EqualFold(device.Name, deviceName) {
				continue
			}
			matches = append(matches, device)
		}
		sort.Slice(matches, func(i, j int) bool {
			iBooted := strings.EqualFold(matches[i].State, "Booted")
			jBooted := strings.EqualFold(matches[j].State, "Booted")
			if iBooted != jBooted {
				return iBooted
			}
			if matches[i].Name != matches[j].Name {
				return matches[i].Name < matches[j].Name
			}
			return matches[i].UDID < matches[j].UDID
		})
		if len(matches) > 0 {
			return candidate, matches[0], nil
		}
	}

	filter := "an available iPhone simulator"
	if deviceName != "" {
		filter = fmt.Sprintf("iPhone simulator %q", deviceName)
	}
	if runtimeFilter != "" {
		filter += fmt.Sprintf(" on iOS runtime %q", runtimeFilter)
	}
	return simulatorRuntime{}, simulatorDevice{}, fmt.Errorf("%s was not found", filter)
}

func compareVersions(left, right string) int {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	count := max(len(leftParts), len(rightParts))
	for i := 0; i < count; i++ {
		var leftValue, rightValue int
		if i < len(leftParts) {
			leftValue, _ = strconv.Atoi(leftParts[i])
		}
		if i < len(rightParts) {
			rightValue, _ = strconv.Atoi(rightParts[i])
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

func formatXcodeVersion(output []byte) string {
	lines := nonEmptyLines(string(output))
	if len(lines) == 0 {
		return "unknown"
	}
	if len(lines) > 1 && strings.HasPrefix(lines[1], "Build version ") {
		return lines[0] + " (" + strings.TrimPrefix(lines[1], "Build version ") + ")"
	}
	return strings.Join(lines, " ")
}

func formatRuntime(runtime simulatorRuntime) string {
	name := runtime.Name
	if name == "" {
		name = "iOS " + runtime.Version
	}
	if runtime.BuildVersion != "" {
		return name + " (" + runtime.BuildVersion + ")"
	}
	return name
}

func failureSummary(summary resultSummary, xcodeOutput []byte, xcodeErr error) string {
	failures := make([]string, 0, len(summary.TestFailures))
	for _, failure := range summary.TestFailures {
		name := failure.TestIdentifierString
		if name == "" {
			name = failure.TestName
		}
		text := strings.TrimSpace(failure.FailureText)
		switch {
		case name != "" && text != "":
			failures = append(failures, name+": "+text)
		case text != "":
			failures = append(failures, text)
		case name != "":
			failures = append(failures, name+" failed")
		}
		if len(failures) == 4 {
			break
		}
	}
	if len(failures) > 0 {
		return truncateDiagnostic(strings.Join(failures, "; "))
	}
	if line := lastNonEmptyLine(xcodeOutput); line != "" {
		return truncateDiagnostic(line)
	}
	if xcodeErr != nil {
		return truncateDiagnostic(xcodeErr.Error())
	}
	return fmt.Sprintf("xcresult outcome is %q with %d failed tests", summary.Result, summary.FailedTests)
}

func commandError(command string, output []byte, err error) string {
	if line := lastNonEmptyLine(output); line != "" {
		return truncateDiagnostic(command + ": " + line)
	}
	return truncateDiagnostic(command + ": " + err.Error())
}

func failedReport(result Result, code, message string) Report {
	return Report{Result: failedResult(result, code, message)}
}

func failedResult(result Result, code, message string) Result {
	result.Passed = false
	result.ErrorCode = code
	result.ErrorMessage = truncateDiagnostic(message)
	result.DiagnosticSummary = result.ErrorMessage
	return result
}

func validateRelativePath(path string) error {
	if filepath.IsAbs(path) {
		return fmt.Errorf("%q must be relative to the stage workspace", path)
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%q must stay inside the stage workspace", path)
	}
	return nil
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func lastNonEmptyLine(output []byte) string {
	lines := nonEmptyLines(string(output))
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

func truncateDiagnostic(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= maxDiagnosticLength {
		return message
	}
	return message[:maxDiagnosticLength-3] + "..."
}
