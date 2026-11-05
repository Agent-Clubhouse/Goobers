package executor

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const passingPlaywrightRecord = "ok 159 [fixture-chromium] > e2e/run-stage-inspector.spec.ts:121:1 > collapsed artifact remains collapsed across three background refreshes (3.8s)\n"

func TestCommandFailureDiagnosticIgnoresSuccessfulTestRecords(t *testing.T) {
	for _, line := range []string{
		passingPlaywrightRecord,
		"\t\x1b[32mok 160 [chromium] › e2e/errors.spec.ts:10:1 › shows Error: unavailable\x1b[0m\r\n",
		"ok 1 - handles error: invalid input at user_test.go:42\n",
		"ok 2 - handles npm error 403 Forbidden - GET https://example.test/package\n",
		"ok - handles AssertionError and panic: failure\n",
		"ok\n",
	} {
		t.Run(strings.TrimSpace(line), func(t *testing.T) {
			if IsFailureLine(line) {
				t.Fatalf("successful test record is a failure finding: %q", line)
			}
			got := summarizeCommandFailure([]byte(line), nil)
			if got.failure.text != "" || len(got.digest) != 0 || got.count != 0 {
				t.Fatalf("successful test record produced failure evidence: %+v", got)
			}
		})
	}
	for _, line := range []string{
		"not ok 160 [chromium] > e2e/errors.spec.ts:10:1 > displays error",
		"ok_test.go:12:1: error: invalid input",
	} {
		if !IsFailureLine(line) {
			t.Fatalf("real failure was suppressed: %q", line)
		}
	}
}

func TestCommandFailureDiagnosticMixedRunners(t *testing.T) {
	for _, failure := range []string{
		"panic: test timed out after 40m0s",
		"panic: runtime error: invalid memory address or nil pointer dereference",
		"fatal error: all goroutines are asleep - deadlock!",
		"FAIL\tgithub.com/goobers/goobers/test/shippedworkflows\t2401.213s",
	} {
		for _, splitStreams := range []bool{false, true} {
			name := failure + "/combined"
			if splitStreams {
				name = failure + "/split"
			}
			t.Run(name, func(t *testing.T) {
				failedOutput := failure + "\n"
				if !strings.HasPrefix(failure, "FAIL") {
					failedOutput += "goroutine 1 [running]:\n\t/usr/local/go/src/testing/testing.go:2682 +0x345\n" +
						"FAIL github.com/goobers/goobers/test/shippedworkflows 2401.213s\n"
				}
				failedOutput += "ci: shipped-workflows: exit status 1\n"
				stdout, stderr := []byte(passingPlaywrightRecord+failedOutput), []byte("make: *** [ci] Error 1\n")
				artifact, start := "local-ci/stdout.log", len(passingPlaywrightRecord)
				data := stdout
				if splitStreams {
					stdout, stderr = []byte(passingPlaywrightRecord), []byte(failedOutput+string(stderr))
					artifact, start, data = "local-ci/stderr.log", 0, stderr
				}

				diagnostic := summarizeCommandFailure(stdout, stderr)
				result := apiv1.ResultEnvelope{Outputs: map[string]interface{}{}, Error: &apiv1.ErrorInfo{Code: "nonzero_exit"}}
				if !applyCommandFailureDiagnostic(&result, 1, diagnostic, "local-ci/stdout.log", "local-ci/stderr.log") {
					t.Fatal("expected a structured failure diagnostic")
				}
				if !strings.Contains(result.Summary, "failure: "+failure) || result.Summary != result.Error.Message {
					t.Fatalf("summary and latest error must identify the Go failure: %+v", result)
				}
				if result.Outputs[outputFailureArtifact] != artifact || int(result.Outputs[outputFailureStartByte].(float64)) != start {
					t.Fatalf("wrong failure artifact or start byte: %+v", result.Outputs)
				}
				end := int(result.Outputs[outputFailureEndByte].(float64))
				if end <= start || end > len(data) || !strings.HasPrefix(string(data[start:end]), failure) {
					t.Fatalf("byte range %d-%d does not identify the Go failure", start, end)
				}
				digest := result.Outputs[outputFailureDigest].(string)
				if !strings.Contains(digest, failure) || strings.Contains(result.Summary+digest, "e2e/") {
					t.Fatalf("failure evidence includes a passing test or omits the Go failure: summary %q, digest %q", result.Summary, digest)
				}
			})
		}
	}
}

func TestCommandFailureDiagnosticExcludesSuccessfulAssertionContext(t *testing.T) {
	const failure = "--- FAIL: TestValidation (0.01s)"
	output := failure + "\nok 1 - handles AssertionError at validation_test.go:10\n"
	got := summarizeCommandFailure([]byte(output), nil).failure
	if got.text != failure || got.start != 0 || got.end != len(failure)+1 {
		t.Fatalf("passing record contaminated failure context: %+v", got)
	}
}
