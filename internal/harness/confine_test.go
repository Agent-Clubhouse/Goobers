package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPrepareCopilotConfinementRefusesToTraverseAWorkspaceSymlink covers
// #2413 for the sandbox runtime directories: they are created in the
// harness's own process, before the confinement they exist to support is
// applied, under a workspace that may hold repository-controlled content. A
// symlink planted at .goobers must not be followed into an arbitrary host
// path.
func TestPrepareCopilotConfinementRefusesToTraverseAWorkspaceSymlink(t *testing.T) {
	workspace := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(workspace, ".goobers")); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareCopilotConfinement(workspace); err == nil {
		t.Fatal("expected prepareCopilotConfinement to refuse to traverse the symlinked .goobers directory")
	}
	if _, err := os.Lstat(filepath.Join(outsideDir, "sandbox")); !os.IsNotExist(err) {
		t.Fatalf("sandbox runtime directory was created outside the workspace through the symlink: err=%v", err)
	}
}

func TestPrepareCopilotConfinementCreatesRuntimeDirectories(t *testing.T) {
	workspace := t.TempDir()

	c, err := prepareCopilotConfinement(workspace)
	if err != nil {
		t.Fatalf("prepareCopilotConfinement: %v", err)
	}
	for _, dir := range []string{c.copilotHome, c.tempDir, c.logDir} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("runtime directory %s missing: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("runtime directory %s permissions = %o, want 700", dir, perm)
		}
	}
	// A second attempt reuses the same directories rather than failing on
	// the ones the first attempt already created.
	if _, err := prepareCopilotConfinement(workspace); err != nil {
		t.Fatalf("prepareCopilotConfinement on an existing runtime tree: %v", err)
	}
}
