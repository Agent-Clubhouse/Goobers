package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerify(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "actions/example@0123456789abcdef0123456789abcdef01234567")
	if err := verify(root); err != nil {
		t.Fatalf("verify pinned workflows: %v", err)
	}
}

func TestVerifyRejectsMutableReference(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkflow(t, root, "actions/example@v1")
	if err := verify(root); err == nil {
		t.Fatal("verify accepted mutable action reference")
	}
}

func writeWorkflow(t *testing.T, root, reference string) {
	t.Helper()
	for _, relativePath := range workflowFiles {
		path := filepath.Join(root, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create workflow directory: %v", err)
		}
		if err := os.WriteFile(path, []byte("      uses: "+reference+"\n"), 0o600); err != nil {
			t.Fatalf("write workflow: %v", err)
		}
	}
}
