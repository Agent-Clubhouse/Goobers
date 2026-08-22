package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// demoWithDocsRoots injects a docsRoots list into the demo workflow and
// returns the instance root, git-initialized with an origin remote naming the
// starter gaggle's spec.project (your-org/your-repo): docsRoots resolve
// against the gaggle's TARGET repository, and the existence check is
// authoritative only when the validated tree is a checkout of it (#3285) —
// the remote is what makes these tests exercise that error path.
func demoWithDocsRoots(t *testing.T, roots []string) string {
	t.Helper()
	root := demoWithDocsRootsNoGit(t, roots)
	// No commit is needed for `git rev-parse --show-toplevel`.
	runGitT(t, root, "init", "-q")
	runGitT(t, root, "remote", "add", "origin", "https://github.com/your-org/your-repo.git")
	return root
}

// demoWithDocsRootsNoGit is demoWithDocsRoots without the git working tree —
// the #3285 shape where no repository exists to resolve roots against.
func demoWithDocsRootsNoGit(t *testing.T, roots []string) string {
	t.Helper()
	root := initDemo(t)

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

// TestValidateWarnsDocsRootsWhenTreeIsNotTargetRepository (#3285): a config
// tree whose git remote is NOT the gaggle's spec.project — the standalone
// workflowSource layout — cannot see the target repository, so a missing
// docs root must warn (naming the target repo), not fail, and validation
// must continue through the DSLVERSION summary the old error suppressed.
func TestValidateWarnsDocsRootsWhenTreeIsNotTargetRepository(t *testing.T) {
	unsetRunContext(t)
	root := demoWithDocsRoots(t, []string{"MISSING.md"})
	runGitT(t, root, "remote", "set-url", "origin", "https://github.com/config-org/workflow-source.git")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (advisory warning); stdout = %q stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `WARNING DOCS003 Workflow/default-implement: declared docs root "MISSING.md" not verified: config tree is not the target repository your-org/your-repo`) {
		t.Fatalf("stdout = %q, want a DOCS003 warning naming the target repository", stdout)
	}
	if !strings.Contains(stdout, "DSLVERSION") {
		t.Fatalf("stdout = %q, want the DSLVERSION summary to render past the warning", stdout)
	}
}

// TestValidateWarnsDocsRootsWhenTreeIsNotGitRepository (#3285): a config tree
// that is not inside a git repository used to SKIP the docs-root check
// silently; it must now warn per declared root instead, and still render the
// DSLVERSION summary.
func TestValidateNotesDocsRootsWhenTreeIsNotGitRepository(t *testing.T) {
	unsetRunContext(t)
	root := demoWithDocsRootsNoGit(t, []string{"MISSING.md"})

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (informational only); stdout = %q stderr = %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "skipped existence check") {
		t.Fatalf("stdout = %q, silent-skip notice must be gone (#3285)", stdout)
	}
	// Not-a-git-repo is the PERMANENT state of every instance root (an
	// instance root is never a checkout), so it must NOT warn — a warning
	// here fires on every `goobers init` forever and is unactionable noise
	// (it broke TestInitThenReferenceWorkflowsValidates for exactly that
	// reason). It prints an informational line instead: visible, never
	// silent, never gating.
	if strings.Contains(stdout, "WARNING DOCS003") {
		t.Fatalf("stdout = %q, a non-git tree must not WARN (instance roots are never checkouts)", stdout)
	}
	if !strings.Contains(stdout, `DOCSROOTS Workflow/default-implement: declared docs root "MISSING.md" not verified here (checked at runtime against your-org/your-repo)`) {
		t.Fatalf("stdout = %q, want the informational not-verified-here line naming the target repository", stdout)
	}
	if !strings.Contains(stdout, "DSLVERSION") {
		t.Fatalf("stdout = %q, want the DSLVERSION summary to render past the notice", stdout)
	}
}

// TestValidateChecksDocsRootsWithScpLikeRemote (#3285): the target-repo match
// must recognize the scp-like remote shape (git@host:owner/name.git), keeping
// the authoritative existence error for checkouts cloned over SSH.
func TestValidateChecksDocsRootsWithScpLikeRemote(t *testing.T) {
	unsetRunContext(t)
	root := demoWithDocsRoots(t, []string{"MISSING.md"})
	runGitT(t, root, "remote", "set-url", "origin", "git@github.com:your-org/your-repo.git")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (missing docs root in target checkout); stdout = %q stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "MISSING.md") || !strings.Contains(stdout, "does not exist") {
		t.Fatalf("stdout = %q, want a 'MISSING.md does not exist' error", stdout)
	}
}
