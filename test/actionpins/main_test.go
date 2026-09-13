package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerify(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflowFile(t, root, "ci.yml", "actions/example@0123456789abcdef0123456789abcdef01234567")
	writeWorkflowFile(t, root, "extra.yaml", "actions/other@abcdef0123456789abcdef0123456789abcdef01")
	if err := verify(root); err != nil {
		t.Fatalf("verify pinned workflows: %v", err)
	}
}

func TestVerifyDiscoversYAMLExtension(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// ci.yml stays pinned here and the mutable reference lives only in a
	// separate .yaml file, so this fails if discovery regresses to the
	// historical hard-coded {ci.yml, release.yml} list or skips .yaml files.
	writeWorkflowFile(t, root, "ci.yml", "actions/example@0123456789abcdef0123456789abcdef01234567")
	writeWorkflowFile(t, root, "additional.yaml", "actions/other@v1")
	if err := verify(root); err == nil {
		t.Fatal("verify accepted mutable action reference in a discovered .yaml workflow")
	}
}

func TestVerifyRejectsMutableReference(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflowFile(t, root, "ci.yml", "actions/example@v1")
	if err := verify(root); err == nil {
		t.Fatal("verify accepted mutable action reference")
	}
}

func TestVerifyAcceptsLocalAction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflowFile(t, root, "ci.yml", "./.github/actions/setup-golangci-lint")
	if err := verify(root); err != nil {
		t.Fatalf("verify local action: %v", err)
	}
}

func TestVerifyAcceptsStepListReference(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Some workflows write "- uses: ..." as a single-line list item rather
	// than "uses:" on its own line; the guard must still see it.
	writeRawWorkflow(t, root, "ci.yml", "      - uses: actions/example@0123456789abcdef0123456789abcdef01234567\n")
	if err := verify(root); err != nil {
		t.Fatalf("verify step-list reference: %v", err)
	}
}

func writeWorkflowFile(t *testing.T, root, name, reference string) {
	t.Helper()
	writeRawWorkflow(t, root, name, "      uses: "+reference+"\n")
}

func writeRawWorkflow(t *testing.T, root, name, contents string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(workflowsDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create workflow directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
}
