package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGaggleWithWorkcopies(t *testing.T, dir, file, name, root string) {
	t.Helper()
	doc := `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: ` + name + `
spec:
  project:
    provider: github
    owner: acme
    name: web
  backlog:
    provider: github
    project: acme/web
  isolation:
    namespace: gaggle-` + name + `
  workcopies:
    root: ` + root + `
`
	if err := os.WriteFile(filepath.Join(dir, file), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func issueWithCode(t *testing.T, report *Report, code WarningCode) (Issue, bool) {
	t.Helper()
	for _, issue := range report.Issues {
		if issue.Code == code && issue.Severity == Error {
			return issue, true
		}
	}
	return Issue{}, false
}

// A relative gaggle workcopies root is refused by canonical validation rather
// than passing here and failing daemon definition construction later (#3663).
func TestGaggleWorkcopiesRootMustBeAbsolute(t *testing.T) {
	dir := t.TempDir()
	writeGaggleWithWorkcopies(t, dir, "gaggle.yaml", "alpha", "relative/workcopies")

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	issue, ok := issueWithCode(t, report, errorWorkcopiesRoot)
	if !ok {
		t.Fatalf("expected %s error, got: %v", errorWorkcopiesRoot, report.Issues)
	}
	if !strings.Contains(issue.Message, "must be an absolute path") {
		t.Fatalf("unexpected message: %q", issue.Message)
	}
	if issue.Name != "alpha" {
		t.Fatalf("expected the issue to name gaggle alpha, got %q", issue.Name)
	}
}

// Two gaggles whose resolved working-copy directories are nested collide: the
// inner gaggle's mirrors and worktrees would live inside the outer gaggle's
// tree.
func TestGaggleWorkcopiesRootCollisionAcrossGaggles(t *testing.T) {
	base := filepath.Join(t.TempDir(), "shared")
	dir := t.TempDir()
	writeGaggleWithWorkcopies(t, dir, "alpha.yaml", "alpha", base)
	writeGaggleWithWorkcopies(t, dir, "beta.yaml", "beta", filepath.Join(base, "alpha"))

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	issue, ok := issueWithCode(t, report, errorWorkcopiesCollision)
	if !ok {
		t.Fatalf("expected %s error, got: %v", errorWorkcopiesCollision, report.Issues)
	}
	if !strings.Contains(issue.Message, "alpha") || !strings.Contains(issue.Message, "collides") {
		t.Fatalf("unexpected message: %q", issue.Message)
	}
}

// Distinct absolute roots — including the common case of a shared base that
// each gaggle's own name is appended beneath — stay clean.
func TestGaggleWorkcopiesRootsWithoutCollision(t *testing.T) {
	base := filepath.Join(t.TempDir(), "shared")
	dir := t.TempDir()
	writeGaggleWithWorkcopies(t, dir, "alpha.yaml", "alpha", base)
	writeGaggleWithWorkcopies(t, dir, "beta.yaml", "beta", base)
	writeGaggleWithWorkcopies(t, dir, "gamma.yaml", "gamma", filepath.Join(base, "elsewhere"))

	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	for _, code := range []WarningCode{errorWorkcopiesRoot, errorWorkcopiesCollision} {
		if issue, ok := issueWithCode(t, report, code); ok {
			t.Fatalf("unexpected %s error: %s", code, issue.Message)
		}
	}
}
