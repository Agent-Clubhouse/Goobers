package flake

import (
	"strconv"
	"strings"
	"testing"
)

// buildBreakOutput is a downstream test's view of one shared compile break:
// its own wrapper message and the relative path it reaches the broken file by
// differ per consumer, the compiler's diagnostics do not.
func buildBreakOutput(wrapper, relative string, line int) string {
	return strings.Join([]string{
		"=== RUN   TestBuild",
		"    build_test.go:41: " + wrapper + ": exit status 1",
		"        # github.com/example/app/cmd/app",
		"        " + relative + "cmd/app/root.go:" + strconv.Itoa(line) + ":9: undefined: newRegistry",
		"        " + relative + "cmd/app/root.go:" + strconv.Itoa(line+7) + ":2: too many arguments in call to run",
		"--- FAIL: TestBuild (2.31s)",
	}, "\n")
}

func TestBuildBreakSignatureCollapsesOneBreakAcrossPackages(t *testing.T) {
	first, ok := BuildBreakSignature(buildBreakOutput("build release docs generator", "../", 120))
	if !ok {
		t.Fatal("compile failure was not recognized as a build break")
	}
	second, ok := BuildBreakSignature(buildBreakOutput("build validator", "../../", 120))
	if !ok {
		t.Fatal("compile failure was not recognized as a build break")
	}
	windows, ok := BuildBreakSignature(strings.ReplaceAll(buildBreakOutput("build gate", `C:\a\app\`, 120), "cmd/app/", `cmd\app\`))
	if !ok {
		t.Fatal("compile failure with Windows paths was not recognized as a build break")
	}
	if first != second || first != windows {
		t.Fatalf("one build break produced different signatures:\n%s\n%s\n%s", first, second, windows)
	}
	for _, unwanted := range []string{"../", "cmd/app", ":120", ":9", "build validator", "github.com/example"} {
		if strings.Contains(first, unwanted) {
			t.Fatalf("signature %q retained volatile %q", first, unwanted)
		}
	}
	for _, wanted := range []string{"root.go: undefined: newRegistry", "root.go: too many arguments in call to run"} {
		if !strings.Contains(first, wanted) {
			t.Fatalf("signature %q lost the compiler diagnostic %q", first, wanted)
		}
	}
	// A line number shift (the same break, re-observed after an unrelated
	// edit above it) is still the same break.
	if shifted, _ := BuildBreakSignature(buildBreakOutput("build validator", "", 160)); shifted != first {
		t.Fatalf("line shift changed the signature: %q vs %q", shifted, first)
	}
}

func TestBuildBreakSignatureSeparatesDifferentErrors(t *testing.T) {
	first, _ := BuildBreakSignature(buildBreakOutput("build validator", "", 120))
	other, ok := BuildBreakSignature(strings.ReplaceAll(buildBreakOutput("build validator", "", 120), "newRegistry", "loadPolicy"))
	if !ok || other == first {
		t.Fatalf("different compile errors share a signature: %q", other)
	}
}

func TestBuildBreakSignatureRejectsNonCompileFailures(t *testing.T) {
	for name, text := range map[string]string{
		"assertion": "=== RUN   TestX\n    x_test.go:12: got 1, want 2\n--- FAIL: TestX (0.00s)",
		"panic":     "panic: boom\n\ngoroutine 7 [running]:\nexample.com/x.f()\n\t/src/x/x.go:12:3 +0x1d",
		// A file:line:col line with no "# package" header is not compiler output.
		"unheaded": "    helper_test.go:12:4: unexpected token",
		"empty":    "",
	} {
		if signature, ok := BuildBreakSignature(text); ok {
			t.Errorf("%s: BuildBreakSignature = %q, want no build break", name, signature)
		}
	}
}

func TestBuildBreakFingerprintKeysOnCommitAndSignature(t *testing.T) {
	signature, _ := BuildBreakSignature(buildBreakOutput("build validator", "", 120))
	base := BuildBreakFingerprint("abc123", signature)
	if got := BuildBreakFingerprint(" abc123 ", signature); got != base {
		t.Fatal("surrounding whitespace on the commit changed the fingerprint")
	}
	if BuildBreakFingerprint("def456", signature) == base {
		t.Fatal("different commits share a build-break fingerprint")
	}
	if BuildBreakFingerprint("abc123", signature+" | other.go: undefined: x") == base {
		t.Fatal("different signatures share a build-break fingerprint")
	}
	if !fingerprintShape(base) {
		t.Fatalf("fingerprint %q is not 64 lowercase hex", base)
	}
}

func fingerprintShape(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// The failures behind #4230 (#4128, #4140) as the stress workflow recorded
// them: the go tool's header survives, the diagnostics do not.
func TestBuildBreakSignatureGroupsHeaderOnlyIncidentFailures(t *testing.T) {
	releaseDocs := "main_test.go:92: run: build release docs generator: exit status 1\n" +
		"# github.com/goobers/goobers/cmd/goobers\n2026-09-01T06:05:53.3006594Z\nstderr:"
	validator := "main_test.go:123: build validator: exit status 1\n# github.com/goobers/goobers/cmd/goobers\nFAIL"
	first, ok := BuildBreakSignature(releaseDocs)
	if !ok {
		t.Fatal("#4128 failure was not recognized as a build break")
	}
	second, ok := BuildBreakSignature(validator)
	if !ok {
		t.Fatal("#4140 failure was not recognized as a build break")
	}
	if first != second || first != "build failed: # github.com/goobers/goobers/cmd/goobers" {
		t.Fatalf("#4128 and #4140 signatures = %q, %q; want one signature naming the broken package", first, second)
	}
	if other, _ := BuildBreakSignature(strings.ReplaceAll(validator, "cmd/goobers", "internal/runner")); other == first {
		t.Fatal("breaks in different packages share a header-only signature")
	}
	// A markdown-style heading is not a go tool header.
	if signature, ok := BuildBreakSignature("    # step one\n    testdata/a.go:3:1: syntax error"); ok {
		t.Fatalf("markdown heading read as a build break: %q", signature)
	}
}
