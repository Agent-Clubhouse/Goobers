package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// addDocsRoots injects a docsRoots list into the demo workflow and returns the
// instance root, git-initialized so `goobers validate`'s existence check has a
// repository tree to resolve roots against.
func demoWithDocsRoots(t *testing.T, roots []string) string {
	t.Helper()
	root := initDemo(t)
	// Make the instance root a git working tree so gitToplevel resolves; no
	// commit is needed for `git rev-parse --show-toplevel`.
	runGitT(t, root, "init", "-q")

	wfPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	raw, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	var block strings.Builder
	block.WriteString("  start: query-backlog\n  docsRoots:\n")
	for _, r := range roots {
		block.WriteString("    - " + r + "\n")
	}
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	updated := strings.Replace(normalized, "  start: query-backlog\n", block.String(), 1)
	if updated == normalized {
		t.Fatalf("demo workflow did not contain the expected start line:\n%s", raw)
	}
	if err := os.WriteFile(wfPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestValidateAcceptsExistingDocsRoots: docs roots that exist in the repository
// pass `goobers validate`.
func TestValidateAcceptsExistingDocsRoots(t *testing.T) {
	unsetRunContext(t)
	root := demoWithDocsRoots(t, []string{"docs", "README.md"})
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout = %q stderr = %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "DOCSROOTS") {
		t.Fatalf("unexpected docs-root complaint: %q", stdout)
	}
}

// TestValidateRejectsMissingDocsRoot: a declared root that does not exist in the
// repository fails validation with a clear message (#1016).
func TestValidateRejectsMissingDocsRoot(t *testing.T) {
	unsetRunContext(t)
	root := demoWithDocsRoots(t, []string{"docs", "MISSING.md"})
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (missing docs root); stdout = %q stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "MISSING.md") || !strings.Contains(stdout, "does not exist") {
		t.Fatalf("stdout = %q, want a 'MISSING.md does not exist' message", stdout)
	}
}

// TestValidateRejectsAbsoluteDocsRoot: the lexical config-load check rejects an
// absolute root before existence is ever consulted.
func TestValidateRejectsAbsoluteDocsRoot(t *testing.T) {
	unsetRunContext(t)
	root := demoWithDocsRoots(t, []string{"/etc/docs"})

	code, stdout, _ := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (absolute docs root); stdout = %q", code, stdout)
	}
	if !strings.Contains(stdout, "docsRoots") {
		t.Fatalf("stdout = %q, want a docsRoots validation error", stdout)
	}
}
