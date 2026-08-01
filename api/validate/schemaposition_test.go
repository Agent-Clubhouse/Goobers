package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// schemaPositionConfig builds a single-gaggle Manifest+Gaggle+Workflow config
// for #2025's schema-position/did-you-mean/cascade-suppression tests.
// gaggleYAML is substituted verbatim as the Gaggle document's own body — the
// caller supplies deliberately broken content to exercise a given failure
// mode. workflowSpecExtra is appended to the Workflow's spec, right after
// `spec.readiness`, to exercise a per-field schema violation there instead.
func schemaPositionConfig(gaggleYAML, workflowSpecExtra string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: schema-position
spec:
  instance:
    name: schema-position
    environment: dev
  gaggles:
    - web
---
` + gaggleYAML + `
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "1.4"
metadata:
  name: query-flow
spec:
  gaggle: web
  triggers:
    - type: manual
  readiness:
    maxConcurrentRuns: 1
` + workflowSpecExtra + `
  start: query
  tasks:
    - name: query
      type: deterministic
      goal: query the backlog
      run:
        command: ["goobers", "backlog-query"]
      capabilities:
        - github:issues:write
`
}

func validGaggleYAML() string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: web
spec:
  project: {provider: github, owner: example, name: web}
  backlog:
    provider: github
    project: example/web
  isolation:
    namespace: web`
}

func writeAndValidate(t *testing.T, content string) *Report {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "instance.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	return report
}

// TestSchemaViolationAdditionalPropertyReportsPositionAndSuggestion is
// #2025's core case: a typo'd top-level field (readines) is rejected with a
// "did you mean" suggestion for the closest allowed field, and the finding
// carries the exact line/column of the offending key — not just the file and
// JSON pointer that were already reported before this feature.
func TestSchemaViolationAdditionalPropertyReportsPositionAndSuggestion(t *testing.T) {
	config := schemaPositionConfig(validGaggleYAML(), "  readines: true\n")
	report := writeAndValidate(t, config)
	if !report.HasErrors() {
		t.Fatal("expected a schema violation for the typo'd field")
	}

	var found *Issue
	for i := range report.Issues {
		if report.Issues[i].Code == errorSchemaViolation {
			found = &report.Issues[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no SCHEMA003 issue in report:\n%s", joinIssues(report))
	}
	if !strings.Contains(found.Message, `did you mean "readiness"?`) {
		t.Errorf("message = %q, want a did-you-mean suggestion for readiness", found.Message)
	}
	if found.Line == 0 {
		t.Errorf("issue = %+v, want a resolved source line, not 0", found)
	}
	// The line/column must point at the actual offending key, not just
	// "somewhere in the file" — count it independently from the fixture text.
	wantLine := strings.Count(config[:strings.Index(config, "readines:")], "\n") + 1
	if found.Line != wantLine {
		t.Errorf("line = %d, want %d (the readines: key's own line)", found.Line, wantLine)
	}

	got := joinIssues(report)
	if !strings.Contains(got, fmt.Sprintf("(line %d, col", wantLine)) {
		t.Errorf("human-readable output missing a (line %d, col N) suffix:\n%s", wantLine, got)
	}
}

// TestSchemaViolationEnumMismatchReportsPosition covers a non-additionalProperties
// keyword (enum): the finding still resolves to the exact node — the bad
// value itself — via direct JSON-pointer walking, with no did-you-mean
// (enum violations aren't near-miss field names).
func TestSchemaViolationEnumMismatchReportsPosition(t *testing.T) {
	gaggle := strings.Replace(validGaggleYAML(), "namespace: web", "namespace: web\n  sandbox:\n    agentic: sideways", 1)
	report := writeAndValidate(t, schemaPositionConfig(gaggle, ""))
	if !report.HasErrors() {
		t.Fatal("expected a schema violation for the bad enum value")
	}
	got := joinIssues(report)
	if !strings.Contains(got, `value must be one of "disabled", "enforced"`) {
		t.Fatalf("missing enum violation:\n%s", got)
	}
	if strings.Contains(got, "did you mean") {
		t.Fatalf("an enum violation must never get a did-you-mean suggestion:\n%s", got)
	}
	if !strings.Contains(got, "(line ") {
		t.Fatalf("enum violation missing a resolved line:\n%s", got)
	}
}

// TestSchemaViolationNoSuggestionWhenNotClose: a field name with no allowed
// field anywhere near it in edit distance gets no did-you-mean guess —
// suggesting something implausible is worse than suggesting nothing.
func TestSchemaViolationNoSuggestionWhenNotClose(t *testing.T) {
	config := schemaPositionConfig(validGaggleYAML(), "  xyzzy123NotCloseToAnything: true\n")
	report := writeAndValidate(t, config)
	got := joinIssues(report)
	if !strings.Contains(got, "additionalProperties") {
		t.Fatalf("expected an additionalProperties violation:\n%s", got)
	}
	if strings.Contains(got, "did you mean") {
		t.Fatalf("an unrelated field name must not get a did-you-mean guess:\n%s", got)
	}
}

// TestReferenceNotFoundSubordinatedAfterParseFailure is #2025's cascade
// case: a Gaggle document with a genuine YAML syntax error leaves the
// Workflow's spec.gaggle reference unresolvable — not because of an
// independent bug, but solely because the Gaggle's own definition never
// parsed. The resulting "no Gaggle/web definition was found" error is
// subordinated with a note pointing back at the real cause, instead of
// reading as a second, unrelated failure.
func TestReferenceNotFoundSubordinatedAfterParseFailure(t *testing.T) {
	broken := validGaggleYAML() + "\n bad: [unterminated\n"
	report := writeAndValidate(t, schemaPositionConfig(broken, ""))
	if !report.HasErrors() {
		t.Fatal("expected the broken Gaggle YAML to fail validation")
	}
	got := joinIssues(report)
	if !strings.Contains(got, "invalid YAML") {
		t.Fatalf("missing the root-cause invalid-YAML error:\n%s", got)
	}
	want := `no Gaggle/web definition was found (a document elsewhere failed to parse as YAML`
	if !strings.Contains(got, want) {
		t.Fatalf("missing subordinated cascade note:\n%s", got)
	}
}

// TestReferenceNotFoundNeverSubordinatedWithoutParseFailure: the same
// "not found" error, with every document parsing fine, must NOT carry the
// cascade note — that would misleadingly suggest a parse failure that never
// happened.
func TestReferenceNotFoundNeverSubordinatedWithoutParseFailure(t *testing.T) {
	config := strings.Replace(schemaPositionConfig(validGaggleYAML(), ""), "gaggle: web", "gaggle: ghost", 1)
	report := writeAndValidate(t, config)
	got := joinIssues(report)
	if !strings.Contains(got, `no Gaggle/ghost definition was found`) {
		t.Fatalf("missing the reference-not-found error:\n%s", got)
	}
	if strings.Contains(got, "a document elsewhere failed to parse") {
		t.Fatalf("must not subordinate a reference error when nothing failed to parse:\n%s", got)
	}
}
