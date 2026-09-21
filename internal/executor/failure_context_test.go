package executor

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFailureContextSmallTools(t *testing.T) {
	for _, finding := range []string{
		"internal/harness/executor.go:418:1: exported method requires comment (revive)",
		"internal/runner/run.go:4909: dispatchTask: cyclomatic complexity grew from 70 to 71",
		"cmd/goobers/applyverdict.go:1932: literal merge-review matches a shipped config name; use a config-sourced workflow role marker",
		"Widget configuration rejected: add a target to the manifest",
	} {
		t.Run(finding, func(t *testing.T) {
			output := finding + "\nexit status 1\nci: custom-check: exit status 1\nmake: *** [Makefile:391: ci] Error 1\n"
			got := summarizeCommandFailure(nil, []byte(output))
			if got.failure.start != 0 || got.failure.end != len(output) {
				t.Fatalf("range = %+v, want whole small artifact", got.failure)
			}
			if !strings.Contains(got.failure.text, finding) || !strings.Contains(strings.Join(got.digest, "\n"), finding) {
				t.Fatalf("missing diagnostic: %+v", got)
			}
		})
	}
}

func TestFailureContextFindsNamedSectionAcrossStreams(t *testing.T) {
	finding := "\x1b[31mUnknown analyser says: configure the widget\x1b[0m\n  expected target: production\n"
	stdout := []byte(strings.Repeat("passed package\n", 5000) + "==> widget-lint\n" + finding + "<== widget-lint (elapsed 1s)\n")
	stderr := []byte("ci: widget-lint: exit status 1\nmake: *** [Makefile:391: ci] Error 1\n")
	got := summarizeCommandFailure(stdout, stderr)
	if got.failure.stream != "stdout" || string(stdout[got.failure.start:got.failure.end]) != finding {
		t.Fatalf("wrong section: %+v", got.failure)
	}
	digest := strings.Join(got.digest, "\n")
	if !strings.Contains(digest, "expected target: production") || strings.Contains(digest, "\x1b") || strings.Contains(digest, "passed package") {
		t.Fatalf("digest = %q", digest)
	}
}

func TestFailureContextPrefersSourceFindingStream(t *testing.T) {
	stdout := []byte("main.go:7:1: add a comment (revive)\n  func Exported() {}\n")
	stderr := []byte("warning: unrelated cache warning\nmake: *** [ci] Error 1\n")
	got := summarizeCommandFailure(stdout, stderr)
	if got.failure.stream != "stdout" || !strings.Contains(got.failure.text, "add a comment") {
		t.Fatalf("wrong stream: %+v", got.failure)
	}
}

func TestFailureEvidenceBounds(t *testing.T) {
	for _, output := range []string{
		strings.Repeat("main.go:3:1: "+strings.Repeat("❯", 100)+"\n", 100),
		"main.go:3:1: " + strings.Repeat("❯", maxFailureDigestBytes),
		strings.Repeat("unknown tool diagnosis ❯ ", maxFailureDigestBytes),
	} {
		got := summarizeCommandFailure([]byte(output), []byte("make: *** [ci] Error 1\n"))
		digest := strings.Join(got.digest, "\n")
		if len(digest) > maxFailureDigestBytes || !utf8.ValidString(digest) || !strings.Contains(digest, "truncated") {
			t.Fatalf("invalid digest: %d bytes, %q", len(digest), digest)
		}
		if got.failure.end-got.failure.start > maxFailureDigestBytes || got.failure.end > len(output) {
			t.Fatalf("invalid range: %+v", got.failure)
		}
	}
}

func TestRecognizedFailureDigestHugeLineBound(t *testing.T) {
	output := []byte("--- FAIL: Test" + strings.Repeat("❯", maxFailureDigestBytes))
	diagnostic := summarizeCommandFailure(output, nil)
	if diagnostic.count != 1 {
		t.Fatalf("truncation marker counted as finding: %d", diagnostic.count)
	}
	digest := strings.Join(diagnostic.digest, "\n")
	if len(digest) > maxFailureDigestBytes || !utf8.ValidString(digest) || !strings.HasPrefix(digest, "--- FAIL: Test") || !strings.Contains(digest, "truncated") {
		t.Fatalf("invalid bounded roster (%d bytes): %q", len(digest), digest)
	}
}

func TestFailureContextRetainsMultipleSourceFindings(t *testing.T) {
	first := "one.go:12:1: add a comment (revive)"
	second := "two.go:32: function grew from 100 to 300 lines"
	stdout := []byte(first + "\n" + strings.Repeat("irrelevant progress\n", 1000) + second + "\n")
	got := summarizeCommandFailure(stdout, []byte("make: *** [ci] Error 1\n"))
	digest := strings.Join(got.digest, "\n")
	if !strings.Contains(digest, first) || !strings.Contains(digest, second) || got.count != 2 {
		t.Fatalf("lost findings: count %d, %q", got.count, digest)
	}
	if got.failure.stream != "stdout" || !strings.Contains(string(stdout[got.failure.start:got.failure.end]), second) {
		t.Fatalf("pointer lost recognized late finding: %+v", got.failure)
	}
}

func TestFailureContextPreservesLateFindingInLargeSection(t *testing.T) {
	finding := "main.go:3:1: export needs comment (revive)"
	stdout := []byte("==> lint\n" + strings.Repeat("Analysing dependency\n", 1000) + finding + "\n<== lint (elapsed 1s)\n")
	got := summarizeCommandFailure(stdout, []byte("ci: lint: exit status 1\nmake: *** [ci] Error 1\n"))
	if !strings.Contains(strings.Join(got.digest, "\n"), finding) || !strings.Contains(got.failure.text, finding) || !strings.Contains(string(stdout[got.failure.start:got.failure.end]), finding) {
		t.Fatalf("lost late recognized finding: %+v", got)
	}
}

func TestFailureContextSourceFindingStraddlesBound(t *testing.T) {
	finding := "main.go:3:1: export needs comment (revive)"
	stdout := []byte("==> lint\n" + strings.Repeat("x", 8170) + "\n" + finding + "\n<== lint (elapsed 1s)\n")
	got := summarizeCommandFailure(stdout, []byte("ci: lint: exit status 1\nmake: *** [ci] Error 1\n"))
	if !strings.Contains(strings.Join(got.digest, "\n"), finding) || !strings.Contains(got.failure.text, finding) || !strings.Contains(string(stdout[got.failure.start:got.failure.end]), finding) {
		t.Fatalf("lost straddling source finding: %+v", got)
	}
}

func TestFailureContextRetainsSectionAlongsideUnknownStderr(t *testing.T) {
	finding := "Unfamiliar analyser requires target production"
	stdout := []byte(strings.Repeat("passed package\n", 5000) + "==> widget-lint\n" + finding + "\n<== widget-lint (elapsed 1s)\n")
	stderr := []byte("unrelated setup message\nci: widget-lint: exit status 1\nmake: *** [ci] Error 1\n")
	got := summarizeCommandFailure(stdout, stderr)
	// Neither arbitrary line can be classified reliably: keep both streams in
	// the digest, and prefer stderr for the single pointer rather than guessing
	// that everything outside stdout's CI framing must be irrelevant.
	digest := strings.Join(got.digest, "\n")
	if !strings.Contains(digest, finding) || !strings.Contains(digest, "unrelated setup message") {
		t.Fatalf("lost ambiguous stream context: %q", digest)
	}
	if got.failure.stream != "stderr" {
		t.Fatalf("ambiguous pointer = %+v", got.failure)
	}
}
