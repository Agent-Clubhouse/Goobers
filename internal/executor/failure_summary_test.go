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

func TestCommandFailureDiagnosticKeepsStdoutDiagnosticOverEqualPriorityStderrTrailer(t *testing.T) {
	// Regression for #3374: a late equal-priority stderr line (here, a make trailer
	// like the one that follows a failed `make ci` recipe) must not steal the
	// recorded failure window from a real diagnostic that already matched in stdout.
	stdout := []byte(`Installing browsers...
Error: EROFS: read-only file system, mkdir '/opt/ms-playwright/__dirlock'
    at Object.mkdirSync (node:fs:1394:26)
Continuing pipeline...
`)
	stderr := []byte(`some unrelated setup logs
make: *** [Makefile:42: ci] Error 2
`)

	got := summarizeCommandFailure(stdout, stderr).failure
	if got.stream != "stdout" {
		t.Fatalf("failure = %+v, want window on stdout, not the equal-priority stderr trailer", got)
	}
	if !strings.Contains(got.text, "EROFS") {
		t.Fatalf("failure = %+v, want the real errno diagnostic", got)
	}
	if strings.Contains(got.text, "Makefile:42") {
		t.Fatalf("failure = %+v, must not be stolen by the stderr make trailer", got)
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

func TestCommandFailureDiagnosticPrefersTransportDenialOverWrapperTrailer(t *testing.T) {
	// Regression for #4143, from production run c1356b5d1a0dd68d3625f2147322290e:
	// `make ci` shells out to `npm ci`, the lockfile named a private mirror that
	// answered 403, and the make trailer that followed won the tie. The recorded
	// diagnostic then said only that a recipe exited, so internal/gate's
	// infrastructure classifier had nothing to recognize and the denial was
	// charged to the item under implementation.
	stderr := []byte(`npm error code E403
npm error 403 403 Forbidden - GET https://ms-feed-12.pkgs.visualstudio.com/1es-public/_packaging/npm-public/npm/registry/three/-/three-0.185.1.tgz
npm error 403 In most cases, you or one of your dependencies are requesting
ci: portal-install: exit status 1
make: *** [Makefile:352: ci] Error 1
`)

	got := summarizeCommandFailure(nil, stderr).failure
	if !strings.Contains(got.text, "403 Forbidden") {
		t.Fatalf("failure = %+v, want the transport denial", got)
	}
	if strings.HasPrefix(got.text, "make: ***") {
		t.Fatalf("failure = %+v, must not select the wrapper trailer", got)
	}
}

func TestCommandFailureDiagnosticPrefersStaleManagedWorktreeOverWrapperTrailer(t *testing.T) {
	stderr := []byte(`level=warning msg="[runner/nolint_filter] Found unknown linters in //nolint directives: wsl"
level=error msg="failed to parse file: open C:\goobers\runs\wt-477de6f81fdc8e7507549359\api\v1alpha1\zz_generated.deepcopy.go: The system cannot find the path specified."
make: *** [Makefile:192: lint-fast] Error 1
`)

	got := summarizeCommandFailure(nil, stderr).failure
	if !strings.Contains(got.text, "wt-477de6f81fdc8e7507549359") {
		t.Fatalf("failure = %+v, want stale managed worktree evidence", got)
	}
	if strings.HasPrefix(got.text, "make: ***") {
		t.Fatalf("failure = %+v, must not select the wrapper trailer", got)
	}
}

// #5101, from the goobernetes cloud instance, run
// ebd455dedd8f54270f9e0eb16c462a9c: 55,805 bytes of stdout whose ONLY failure
// signal was a package-level "FAIL\tpkg\t1315.643s" — no "--- FAIL:", no
// panic, no timeout — while stderr held 3,931 bytes of module-download noise
// ending in make's trailer. The diagnostic picked stderr, so the implementer
// was told nothing it could act on and repassed blind until its budget was
// exhausted.
func TestPackageLevelFailureOutranksWrapperTrailer(t *testing.T) {
	stdout := []byte("go: downloading k8s.io/component-base v0.37.0\n" +
		"ok  \tgithub.com/goobers/goobers/internal/journal\t1.2s\n" +
		"FAIL\tgithub.com/goobers/goobers/cmd/goobers\t1315.643s\n" +
		"FAIL\n")
	stderr := []byte("npm notice changelog\nexit status 1\nci: test: exit status 1\nmake: *** [Makefile:391: ci] Error 1\n")

	got := FailureDiagnostic(stdout, stderr)
	if !strings.Contains(got, "cmd/goobers") {
		t.Fatalf("diagnostic does not name the failing package, so a repass sees nothing actionable: %q", got)
	}
	if strings.Contains(got, "make: ***") {
		t.Fatalf("wrapper trailer displaced the package failure: %q", got)
	}
}

// The repass roster must carry EVERY failing test, across packages, not just
// the first — that is the defect #5101 names.
func TestFailureDigestCarriesEveryFailure(t *testing.T) {
	stdout := []byte("--- FAIL: TestAlpha (0.01s)\n" +
		"    alpha_test.go:10: want 1 got 2\n" +
		"--- FAIL: TestBeta (0.02s)\n" +
		"    beta_test.go:20: boom\n" +
		"FAIL\tgithub.com/goobers/goobers/internal/one\t3.1s\n" +
		"--- FAIL: TestGamma (0.03s)\n" +
		"FAIL\tgithub.com/goobers/goobers/internal/two\t4.2s\n" +
		"FAIL\n")
	stderr := []byte("make: *** [Makefile:391: ci] Error 1\n")

	digest := summarizeCommandFailure(stdout, stderr).digest
	joined := strings.Join(digest, "\n")
	for _, want := range []string{"TestAlpha", "TestBeta", "TestGamma", "internal/one", "internal/two"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("digest omits %q — a repass would fix what it can see and meet the rest next attempt:\n%s", want, joined)
		}
	}
	// Per-test failures are more specific than package verdicts and must lead.
	if !strings.HasPrefix(digest[0], "--- FAIL:") {
		t.Fatalf("digest does not lead with the most specific failure: %q", digest[0])
	}
	// Wrapper trailers name nothing and must not consume the bound.
	if strings.Contains(joined, "make: ***") {
		t.Fatalf("digest kept the wrapper trailer:\n%s", joined)
	}
	if len(joined) > maxFailureDigestBytes {
		t.Fatalf("digest = %d bytes, exceeds the documented bound %d", len(joined), maxFailureDigestBytes)
	}
}

// Duplicates are common (a retried package reprints its verdict) and must not
// crowd out distinct failures at the bound.
func TestFailureDigestDeduplicates(t *testing.T) {
	line := "--- FAIL: TestRepeated (0.01s)\n"
	stdout := []byte(strings.Repeat(line, 5) + "FAIL\tgithub.com/goobers/goobers/internal/one\t1.0s\n")
	digest := summarizeCommandFailure(stdout, nil).digest
	count := 0
	for _, entry := range digest {
		if strings.Contains(entry, "TestRepeated") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("TestRepeated appears %d times, want 1: %v", count, digest)
	}
}

// A build that dies before any test runs still has to say something: the
// wrapper trailer is dropped only when something more specific exists.
func TestFailureDigestKeepsTrailerWhenItIsAllThereIs(t *testing.T) {
	digest := summarizeCommandFailure(nil, []byte("make: *** [Makefile:391: ci] Error 1\n")).digest
	if len(digest) == 0 {
		t.Fatal("digest is empty for a build that failed before any test ran")
	}
	if !strings.Contains(strings.Join(digest, "\n"), "make: ***") {
		t.Fatalf("digest dropped the only failure signal available: %v", digest)
	}
}
