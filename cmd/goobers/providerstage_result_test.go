package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

func TestProviderChainCommandsWriteEarlyFailureResult(t *testing.T) {
	tests := []struct {
		command     string
		errorReason string
	}{
		{command: "apply-verdict", errorReason: "selectedNumber is required"},
		{command: "backlog-query", errorReason: "instance.yaml"},
		{command: "elect-lander", errorReason: "selectedNumber is required"},
		{command: "gather-pr-context", errorReason: "instance.yaml"},
		{command: "gather-sibling-context", errorReason: "instance.yaml"},
		{command: "issue-close-out", errorReason: "instance.yaml"},
		{command: "merge-pr", errorReason: "instance.yaml"},
		{command: "merge-queue-poll", errorReason: "instance.yaml"},
		{command: "open-pr", errorReason: "instance.yaml"},
		{command: "post-merge", errorReason: "instance.yaml"},
		{command: "pr-select", errorReason: "instance.yaml"},
		{command: "rebase-pr", errorReason: "selectedNumber and head are required"},
		{command: "remediation-checkpoint", errorReason: "GOOBERS_RUN_ID is not set"},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			unsetRunContext(t)
			t.Setenv(executor.InputEnvVar("selectedNumber"), "")
			t.Setenv(executor.InputEnvVar("head"), "")

			resultFile := filepath.Join(t.TempDir(), "result.json")
			t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
			if err := os.WriteFile(resultFile, []byte(`{"status":"stale-success"}`), 0o644); err != nil {
				t.Fatalf("seed stale result file: %v", err)
			}
			missingRoot := filepath.Join(t.TempDir(), "missing-instance")

			code, _, stderr := runArgs(t, tt.command, missingRoot)
			if code != 1 {
				t.Fatalf("code = %d, stderr = %q, want 1", code, stderr)
			}

			data, err := os.ReadFile(resultFile)
			if err != nil {
				t.Fatalf("read result file: %v", err)
			}
			var result map[string]interface{}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatalf("decode result file: %v", err)
			}
			if result[executor.OutputErrorCode] != errorCodeProvider {
				t.Fatalf("errorCode = %v, want %s", result[executor.OutputErrorCode], errorCodeProvider)
			}
			if _, ok := result["status"]; ok {
				t.Fatalf("provider-stage result retained stale invocation data: %v", result)
			}
			message, _ := result[executor.OutputErrorMessage].(string)
			if !strings.Contains(message, tt.errorReason) {
				t.Fatalf("errorMessage = %q, want it to contain %q", message, tt.errorReason)
			}
		})
	}
}

func TestProviderChainCommandsUseDefaultResultFile(t *testing.T) {
	tests := []struct {
		command     string
		resultFile  string
		errorReason string
	}{
		{command: "issue-close-out", resultFile: "issue-close-out-result.json", errorReason: "instance.yaml"},
		{command: "post-merge", resultFile: "post-merge-result.json", errorReason: "instance.yaml"},
		{command: "remediation-checkpoint", resultFile: "checkpoint-result.json", errorReason: "GOOBERS_RUN_ID is not set"},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			unsetRunContext(t)
			resultEnv := executor.InputEnvVar(executor.InputResultFile)
			original, hadOriginal := os.LookupEnv(resultEnv)
			if err := os.Unsetenv(resultEnv); err != nil {
				t.Fatalf("unset %s: %v", resultEnv, err)
			}
			t.Cleanup(func() {
				if hadOriginal {
					_ = os.Setenv(resultEnv, original)
				} else {
					_ = os.Unsetenv(resultEnv)
				}
			})

			workDir := t.TempDir()
			t.Chdir(workDir)
			missingRoot := filepath.Join(t.TempDir(), "missing-instance")

			code, _, stderr := runArgs(t, tt.command, missingRoot)
			if code != 1 {
				t.Fatalf("code = %d, stderr = %q, want 1", code, stderr)
			}
			result := readProviderStageResult(t, filepath.Join(workDir, tt.resultFile))
			if result[executor.OutputErrorCode] != errorCodeProvider {
				t.Fatalf("errorCode = %v, want %s", result[executor.OutputErrorCode], errorCodeProvider)
			}
			message, _ := result[executor.OutputErrorMessage].(string)
			if !strings.Contains(message, tt.errorReason) {
				t.Fatalf("errorMessage = %q, want it to contain %q", message, tt.errorReason)
			}
		})
	}
}

func TestProviderStageCommandWritesEmptySuccessResult(t *testing.T) {
	resultFile := filepath.Join(t.TempDir(), "result.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
	code := runProviderStageCommand(
		"test-provider-stage",
		"",
		func(_ []string, _, _ io.Writer) int { return 0 },
		nil,
		io.Discard,
		io.Discard,
	)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	assertEmptyProviderStageResult(t, resultFile)
}

func TestProviderStageCommandPreservesTransientRetryability(t *testing.T) {
	resultFile := filepath.Join(t.TempDir(), "result.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
	code := runProviderStageCommand(
		"test-provider-stage",
		"",
		func(_ []string, _, stderr io.Writer) int {
			pln(stderr, "error: git fetch: connection refused")
			return 1
		},
		nil,
		io.Discard,
		io.Discard,
	)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	result := readProviderStageResult(t, resultFile)
	if result[executor.OutputErrorCode] != errorCodeNetwork {
		t.Fatalf("errorCode = %v, want %s", result[executor.OutputErrorCode], errorCodeNetwork)
	}
	if result[executor.OutputErrorRetryable] != true {
		t.Fatalf("errorRetryable = %v, want true", result[executor.OutputErrorRetryable])
	}
}

func readProviderStageResult(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read provider-stage result: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode provider-stage result: %v", err)
	}
	return result
}

func assertEmptyProviderStageResult(t *testing.T, path string) {
	t.Helper()
	result := readProviderStageResult(t, path)
	if len(result) != 0 {
		t.Fatalf("provider-stage result = %v, want empty outputs", result)
	}
}

func assertNoWorkProviderStageResult(t *testing.T, path string) {
	t.Helper()
	result := readProviderStageResult(t, path)
	if result[executor.OutputNoWork] != true {
		t.Fatalf("provider-stage result = %v, want noWork=true", result)
	}
}
