package executor

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCommandFailureDiagnosticPrefersLaterVitestFailureOverSidebarWarnings(t *testing.T) {
	stderr := []byte(strings.Repeat("Warning: An update to Sidebar inside a test was not wrapped in act(...)\n", 12))
	stdout := []byte(` Test Files  1 failed | 18 passed
 FAIL  src/goobers/agent-instructions-validation.test.ts > do not contain copied repository or Go-only guidance
AssertionError: expected guidance not to contain 'make ci'
 ❯ src/goobers/agent-instructions-validation.test.ts:44:18
`)

	diagnostic := summarizeCommandFailure(stdout, stderr)
	result := apiv1.ResultEnvelope{
		Outputs: map[string]interface{}{},
		Error:   &apiv1.ErrorInfo{Code: "nonzero_exit"},
	}
	if !applyCommandFailureDiagnostic(&result, 1, diagnostic, "local-ci/stdout.log", "local-ci/stderr.log") {
		t.Fatal("expected a structured failure diagnostic")
	}

	for _, want := range []string{
		"agent-instructions-validation.test.ts > do not contain copied repository or Go-only guidance",
		"AssertionError: expected guidance not to contain 'make ci'",
		"warnings: separate local-ci/stderr.log evidence",
	} {
		if !strings.Contains(result.Summary, want) {
			t.Fatalf("summary = %q, want %q", result.Summary, want)
		}
	}
	if strings.Contains(result.Summary, "Sidebar") {
		t.Fatalf("summary = %q, must not present the unrelated warning as the failure", result.Summary)
	}
	if result.Outputs[outputFailureArtifact] != "local-ci/stdout.log" {
		t.Fatalf("outputs = %+v, want stdout failure artifact", result.Outputs)
	}
	if result.Outputs[outputWarningArtifact] != "local-ci/stderr.log" {
		t.Fatalf("outputs = %+v, want separately labeled stderr warning artifact", result.Outputs)
	}
	start := int(result.Outputs[outputFailureStartByte].(float64))
	end := int(result.Outputs[outputFailureEndByte].(float64))
	if evidence := string(stdout[start:end]); !strings.Contains(evidence, "AssertionError") {
		t.Fatalf("failure byte range = %q, want complete decisive section", evidence)
	}
}

func TestCommandFailureDiagnosticRecognizesCommonRunnerFormats(t *testing.T) {
	tests := []struct {
		name, output, want string
	}{
		{
			name:   "jest",
			output: "FAIL src/user.test.ts\n  user validation\n    Expected: true\n    Received: false\n",
			want:   "FAIL src/user.test.ts",
		},
		{
			name:   "go test",
			output: "--- FAIL: TestUserValidation (0.00s)\n    user_test.go:42: got false, want true\nFAIL\tgithub.com/example/users\t0.01s\n",
			want:   "--- FAIL: TestUserValidation",
		},
		{
			name:   "dotnet",
			output: "Failed UserServiceTests.ValidatesUser [12 ms]\nError Message:\n Assert.True() Failure\n",
			want:   "Assert.True() Failure",
		},
		{
			name:   "compiler",
			output: "src/main.ts(12,4): error TS2322: Type 'string' is not assignable to type 'number'.\n",
			want:   "error TS2322",
		},
		{
			name:   "go compiler",
			output: "# github.com/example/users\n./users.go:12: undefined: userID\n",
			want:   "users.go:12: undefined: userID",
		},
		{
			name:   "build target",
			output: "make: *** [Makefile:12: build] Error 2\n",
			want:   "Makefile:12: build",
		},
		{
			name:   "rust compiler",
			output: "error[E0308]: mismatched types\n",
			want:   "error[E0308]",
		},
		{
			name:   "maven",
			output: "[ERROR] Failed to execute goal org.apache.maven.plugins:maven-compiler-plugin:compile\n",
			want:   "maven-compiler-plugin:compile",
		},
		{
			name:   "gradle target",
			output: "Execution failed for task ':app:compileJava'.\n> Compilation failed\nBUILD FAILED in 2s\n",
			want:   "Execution failed for task ':app:compileJava'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeCommandFailure([]byte(tt.output), nil).failure
			if !strings.Contains(got.text, tt.want) {
				t.Fatalf("failure = %+v, want %q", got, tt.want)
			}
			if got.start < 0 || got.end <= got.start || got.end > len(tt.output) {
				t.Fatalf("failure range = %d-%d for %d-byte output", got.start, got.end, len(tt.output))
			}
		})
	}
}

func TestCommandFailureDiagnosticPrefersFinalSpecificFailure(t *testing.T) {
	output := []byte(`FAIL src/earlier.test.ts > reports an earlier assertion
AssertionError: expected true
./users.go:12: undefined: userID
`)

	got := summarizeCommandFailure(output, nil).failure
	if !strings.Contains(got.text, "users.go:12: undefined: userID") {
		t.Fatalf("failure = %+v, want final compiler failure", got)
	}
	if strings.Contains(got.text, "earlier.test.ts") {
		t.Fatalf("failure = %+v, must not select earlier test failure", got)
	}
}

func TestCommandFailureDiagnosticIgnoresAggregateFailedTestsHeading(t *testing.T) {
	output := []byte("FAIL src/first.test.ts > reports the assertion\nAssertionError: got false\nFailed Tests 1\n")
	got := summarizeCommandFailure(output, nil).failure
	if !strings.Contains(got.text, "src/first.test.ts > reports the assertion") {
		t.Fatalf("failure = %+v, want the specific failing test", got)
	}
}

func TestCommandFailureDiagnosticUsesSafeGenericFallback(t *testing.T) {
	diagnostic := summarizeCommandFailure(nil, []byte("an unfamiliar tool failed\n"))
	if diagnostic.failure.text != "" {
		t.Fatalf("diagnostic = %+v, want no format-specific match", diagnostic)
	}
}

func TestBoundDiagnosticPreservesUTF8(t *testing.T) {
	got := boundDiagnostic(strings.Repeat("❯", maxFailureSummaryBytes))
	if !strings.Contains(got, "...") || strings.ToValidUTF8(got, "") != got {
		t.Fatalf("bounded diagnostic is not valid truncated UTF-8: %q", got)
	}
}
