package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeADOGaggleWithDoneStates writes a minimal ADO gaggle whose backlog
// block carries the given doneStates YAML (indented under backlog).
func writeADOGaggleWithDoneStates(t *testing.T, dir, doneStates string) {
	t.Helper()
	doc := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: alpha
spec:
  project:
    provider: ado
    owner: example-org
    project: example-project
    name: web
  backlog:
    provider: ado
    project: example-project
` + doneStates + `
  isolation:
    namespace: gaggle-alpha
`
	if err := os.WriteFile(filepath.Join(dir, "gaggle.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBacklogDoneStatesValidatesCleanly pins ADO-N32's additive gaggle
// setting: a well-formed backlog.doneStates is accepted with no issue of any
// severity, so declaring it never raises a version or schema warning.
func TestBacklogDoneStatesValidatesCleanly(t *testing.T) {
	dir := t.TempDir()
	writeADOGaggleWithDoneStates(t, dir, `    doneStates:
      categories: [Resolved, Completed, Removed]
      byType:
        Bug: [Closed]`)

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	for _, issue := range report.Issues {
		if strings.Contains(issue.Message, "doneStates") || strings.HasPrefix(string(issue.Code), "SCHEMA") || strings.HasPrefix(string(issue.Code), "VER") {
			t.Fatalf("unexpected issue for a valid doneStates: %+v", issue)
		}
	}
}

// TestBacklogDoneStatesRejectsUnknownCategory: an unknown state category
// name is an error (docs/design/ado-parity-dsl-2-0.md §6), not a silent
// no-op that would leave every predecessor blocking.
func TestBacklogDoneStatesRejectsUnknownCategory(t *testing.T) {
	dir := t.TempDir()
	writeADOGaggleWithDoneStates(t, dir, `    doneStates:
      categories: [Done]`)

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	issue, ok := issueWithCodeAndSeverity(t, report, errorSchemaViolation, Error)
	if !ok {
		t.Fatalf("expected a %s error for an unknown category, got: %v", errorSchemaViolation, report.Issues)
	}
	if !strings.Contains(issue.Message, "doneStates") {
		t.Fatalf("schema error does not point at doneStates: %+v", issue)
	}
}
