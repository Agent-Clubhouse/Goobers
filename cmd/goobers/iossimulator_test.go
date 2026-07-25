package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/iossimulator"
)

func TestIOSSimulatorCommandRejectsNonMacOSBeforeToolInvocation(t *testing.T) {
	resultFile := "ios-result.json"
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
	t.Chdir(t.TempDir())
	tools := &countingIOSTools{}

	var stderr strings.Builder
	code := runIOSSimulatorTestWith(
		[]string{"--project", "App.xcodeproj", "--scheme", "AppUITests"},
		io.Discard,
		&stderr,
		iosSimulatorCommandDeps{hostOS: "linux", tools: tools},
	)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1", code, stderr.String())
	}
	if tools.calls != 0 {
		t.Fatalf("tool calls = %d, want none before host rejection", tools.calls)
	}
	if !strings.Contains(stderr.String(), "require macOS") {
		t.Fatalf("stderr = %q, want macOS diagnostic", stderr.String())
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var result iossimulator.Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ErrorCode != "unsupported_host" || result.Passed {
		t.Fatalf("result = %+v, want unsupported-host failure", result)
	}
}

func TestIOSSimulatorCommandRejectsEscapingResultPath(t *testing.T) {
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), "../result.json")
	var stderr strings.Builder
	code := runIOSSimulatorTestWith(
		[]string{"--project", "App.xcodeproj", "--scheme", "AppUITests"},
		io.Discard,
		&stderr,
		iosSimulatorCommandDeps{hostOS: "linux", tools: &countingIOSTools{}},
	)
	if code != 2 || !strings.Contains(stderr.String(), "must stay inside") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestIOSSimulatorCommandRequiresProjectOrWorkspace(t *testing.T) {
	var stderr strings.Builder
	code := runIOSSimulatorTestWith(
		[]string{"--scheme", "AppUITests"},
		io.Discard,
		&stderr,
		iosSimulatorCommandDeps{hostOS: "darwin", tools: &countingIOSTools{}},
	)
	if code != 2 || !strings.Contains(stderr.String(), "exactly one") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

type countingIOSTools struct {
	calls int
}

func (c *countingIOSTools) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	c.calls++
	return nil, nil
}
