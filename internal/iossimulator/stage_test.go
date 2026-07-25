package iossimulator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type scriptedCall struct {
	name   string
	args   []string
	output string
	err    error
}

type scriptedTools struct {
	t     *testing.T
	calls []scriptedCall
}

func (s *scriptedTools) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.t.Helper()
	if len(s.calls) == 0 {
		s.t.Fatalf("unexpected command: %s %v", name, args)
	}
	call := s.calls[0]
	s.calls = s.calls[1:]
	if name != call.name || !reflect.DeepEqual(args, call.args) {
		s.t.Fatalf("command = %s %v, want %s %v", name, args, call.name, call.args)
	}
	return []byte(call.output), call.err
}

func TestRunPassesParsedXCResultAndRecordsVersions(t *testing.T) {
	tools := successfulScript(t, `{
		"result":"Passed","totalTestCount":1,"passedTests":1,
		"failedTests":0,"skippedTests":0,"testFailures":[]
	}`, nil)

	report := Run(context.Background(), Options{
		Project: "GoobersIOS.xcodeproj", Scheme: "GoobersIOS",
		OnlyTesting: "GoobersIOSUITests", ResultBundle: "artifacts/tests.xcresult",
	}, tools)

	if !report.Result.Passed || report.Result.Outcome != "Passed" {
		t.Fatalf("result = %+v, want passed xcresult", report.Result)
	}
	if report.Result.XcodeVersion != "Xcode 26.0.1 (17A400)" {
		t.Errorf("xcodeVersion = %q", report.Result.XcodeVersion)
	}
	if report.Result.SimulatorRuntime != "iOS 26.0 (23A123)" || report.Result.SimulatorName != "iPhone 17 Pro" {
		t.Errorf("simulator selection = %q / %q", report.Result.SimulatorRuntime, report.Result.SimulatorName)
	}
	if report.Result.TotalTests != 1 || report.Result.PassedTests != 1 {
		t.Errorf("test counts = total %d passed %d", report.Result.TotalTests, report.Result.PassedTests)
	}
	if len(tools.calls) != 0 {
		t.Fatalf("unconsumed commands: %+v", tools.calls)
	}
}

func TestRunReturnsTestFailureWithXCResultDiagnostics(t *testing.T) {
	tools := successfulScript(t, `{
		"result":"Failed","totalTestCount":1,"passedTests":0,
		"failedTests":1,"skippedTests":0,
		"testFailures":[{
			"testName":"testLaunchesRepresentativeTarget()",
			"targetName":"GoobersIOSUITests",
			"failureText":"XCTAssertTrue failed - ready label was missing",
			"testIdentifierString":"SmokeUITests/testLaunchesRepresentativeTarget()"
		}]
	}`, errors.New("exit status 65"))

	report := Run(context.Background(), Options{
		Project: "GoobersIOS.xcodeproj", Scheme: "GoobersIOS",
	}, tools)

	if report.Result.Passed || report.Result.ErrorCode != "ios_tests_failed" {
		t.Fatalf("result = %+v, want typed test failure", report.Result)
	}
	for _, want := range []string{"SmokeUITests/testLaunchesRepresentativeTarget()", "ready label was missing"} {
		if !strings.Contains(report.Result.DiagnosticSummary, want) {
			t.Errorf("diagnostic %q does not contain %q", report.Result.DiagnosticSummary, want)
		}
	}
	if report.Result.FailedTests != 1 || len(report.SummaryOutput) == 0 {
		t.Errorf("failure report lost xcresult evidence: %+v", report)
	}
}

func TestRunRejectsXcode15BeforeSimulatorOrTestInvocation(t *testing.T) {
	tools := &scriptedTools{t: t, calls: []scriptedCall{
		{name: "xcodebuild", args: []string{"-version"}, output: "Xcode 15.4\nBuild version 15F31d\n"},
	}}

	report := Run(context.Background(), Options{
		Project: "GoobersIOS.xcodeproj", Scheme: "GoobersIOS",
	}, tools)

	if report.Result.ErrorCode != "xcode_version_unsupported" || report.Result.Passed {
		t.Fatalf("result = %+v, want unsupported Xcode failure", report.Result)
	}
	for _, want := range []string{"Xcode 16 or newer", "Xcode 15.4 (15F31d)"} {
		if !strings.Contains(report.Result.ErrorMessage, want) {
			t.Errorf("error %q does not contain %q", report.Result.ErrorMessage, want)
		}
	}
	if len(tools.calls) != 0 {
		t.Fatalf("commands ran after Xcode version rejection: %+v", tools.calls)
	}
}

func TestRunFailsClearlyWhenRequestedSimulatorIsUnavailable(t *testing.T) {
	tools := &scriptedTools{t: t, calls: []scriptedCall{
		{name: "xcodebuild", args: []string{"-version"}, output: "Xcode 26.0\nBuild version 17A400\n"},
		{name: "xcrun", args: []string{"simctl", "list", "runtimes", "available", "--json"}, output: runtimeFixture},
		{name: "xcrun", args: []string{"simctl", "list", "devices", "available", "--json"}, output: deviceFixture},
	}}

	report := Run(context.Background(), Options{
		Project: "GoobersIOS.xcodeproj", Scheme: "GoobersIOS", Device: "iPhone 99",
	}, tools)
	if report.Result.ErrorCode != "simulator_not_found" || !strings.Contains(report.Result.ErrorMessage, "iPhone 99") {
		t.Fatalf("result = %+v, want unavailable-device diagnostic", report.Result)
	}
}

func TestRunFailsClosedWhenXCResultContainsNoTests(t *testing.T) {
	tools := successfulScript(t, `{
		"result":"Passed","totalTestCount":0,"passedTests":0,
		"failedTests":0,"skippedTests":0,"testFailures":[]
	}`, nil)

	report := Run(context.Background(), Options{
		Project: "GoobersIOS.xcodeproj", Scheme: "GoobersIOS",
		OnlyTesting: "GoobersIOSUITests", ResultBundle: "artifacts/tests.xcresult",
	}, tools)
	if report.Result.ErrorCode != "ios_tests_not_found" || report.Result.Passed {
		t.Fatalf("result = %+v, want zero-test failure", report.Result)
	}
}

func TestValidateHostAndOptionsFailClosed(t *testing.T) {
	if err := ValidateHost("linux"); err == nil || !strings.Contains(err.Error(), "macOS") {
		t.Fatalf("ValidateHost(linux) = %v", err)
	}
	if err := ValidateHost("darwin"); err != nil {
		t.Fatalf("ValidateHost(darwin) = %v", err)
	}
	for _, options := range []Options{
		{},
		{Project: "a.xcodeproj", Workspace: "a.xcworkspace", Scheme: "Tests"},
		{Project: "a.xcodeproj"},
		{Project: "a.xcodeproj", Scheme: "Tests", ResultBundle: "../outside.xcresult"},
	} {
		if err := ValidateOptions(options); err == nil {
			t.Errorf("ValidateOptions(%+v) unexpectedly passed", options)
		}
	}
}

func successfulScript(t *testing.T, summary string, xcodeErr error) *scriptedTools {
	t.Helper()
	bundle := DefaultResultBundle
	xcodeArgs := []string{
		"test", "-project", "GoobersIOS.xcodeproj", "-scheme", "GoobersIOS",
		"-destination", "platform=iOS Simulator,id=IPHONE-17",
		"-resultBundlePath", bundle,
		"-parallel-testing-enabled", "NO",
	}
	if xcodeErr == nil {
		bundle = "artifacts/tests.xcresult"
		xcodeArgs[8] = bundle
		xcodeArgs = append(xcodeArgs, "-only-testing:GoobersIOSUITests")
	}
	return &scriptedTools{t: t, calls: []scriptedCall{
		{name: "xcodebuild", args: []string{"-version"}, output: "Xcode 26.0.1\nBuild version 17A400\n"},
		{name: "xcrun", args: []string{"simctl", "list", "runtimes", "available", "--json"}, output: runtimeFixture},
		{name: "xcrun", args: []string{"simctl", "list", "devices", "available", "--json"}, output: deviceFixture},
		{name: "xcrun", args: []string{"simctl", "boot", "IPHONE-17"}},
		{name: "xcrun", args: []string{"simctl", "bootstatus", "IPHONE-17", "-b"}},
		{name: "xcodebuild", args: xcodeArgs, output: "** TEST EXECUTE SUCCEEDED **\n", err: xcodeErr},
		{name: "xcrun", args: []string{"xcresulttool", "get", "test-results", "summary", "--path", bundle, "--compact"}, output: summary},
	}}
}

const runtimeFixture = `{
	"runtimes":[
		{"identifier":"com.apple.CoreSimulator.SimRuntime.iOS-18-2","name":"iOS 18.2","version":"18.2","buildversion":"22C150","isAvailable":true},
		{"identifier":"com.apple.CoreSimulator.SimRuntime.iOS-26-0","name":"iOS 26.0","version":"26.0","buildversion":"23A123","isAvailable":true}
	]
}`

const deviceFixture = `{
	"devices":{
		"com.apple.CoreSimulator.SimRuntime.iOS-18-2":[
			{"udid":"IPHONE-16","name":"iPhone 16","state":"Booted","deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-16","isAvailable":true}
		],
		"com.apple.CoreSimulator.SimRuntime.iOS-26-0":[
			{"udid":"IPAD","name":"iPad Pro","state":"Shutdown","deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPad-Pro","isAvailable":true},
			{"udid":"IPHONE-17","name":"iPhone 17 Pro","state":"Shutdown","deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro","isAvailable":true}
		]
	}
}`

func TestCompareVersions(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"26.0", "18.2", 1},
		{"18.2", "18.2.0", 0},
		{"17.4.1", "17.4.2", -1},
	} {
		t.Run(fmt.Sprintf("%s_%s", test.left, test.right), func(t *testing.T) {
			if got := compareVersions(test.left, test.right); got != test.want {
				t.Fatalf("compareVersions(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
			}
		})
	}
}
