package harness

import (
	"os"
	"path/filepath"
	"runtime"
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
		// Unix mode bits are meaningless on NTFS: os.Mkdir's perm argument
		// only toggles the read-only attribute there, and a directory always
		// surfaces back as 0777 — so the privacy of these directories is not
		// something Perm() can assert on Windows.
		if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o700 {
			t.Fatalf("runtime directory %s permissions = %o, want 700", dir, perm)
		}
	}
	// A second attempt reuses the same directories rather than failing on
	// the ones the first attempt already created.
	if _, err := prepareCopilotConfinement(workspace); err != nil {
		t.Fatalf("prepareCopilotConfinement on an existing runtime tree: %v", err)
	}
}

// TestPrepareCopilotConfinementKeepsTheWorkspaceSpelling pins that the
// runtime directories are reported under the workspace root the caller
// passed, not the canonicalized root safepath resolves through. These paths
// become the confined subprocess's COPILOT_HOME, TMPDIR and --log-dir, next
// to a sandbox policy expressed in terms of the caller's workspace; on macOS
// (/var -> /private/var) and Windows (8.3 short paths) a canonicalized
// prefix would point the CLI's runtime state at a path the policy never
// named.
func TestPrepareCopilotConfinementKeepsTheWorkspaceSpelling(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), workspace); err != nil {
		t.Fatal(err)
	}

	c, err := prepareCopilotConfinement(workspace)
	if err != nil {
		t.Fatalf("prepareCopilotConfinement: %v", err)
	}
	base := filepath.Join(workspace, ".goobers", "sandbox")
	for name, got := range map[string]string{
		"copilot-home": c.copilotHome,
		"tmp":          c.tempDir,
		"logs":         c.logDir,
	} {
		if want := filepath.Join(base, name); got != want {
			t.Fatalf("runtime directory = %q, want %q", got, want)
		}
		if info, err := os.Stat(got); err != nil || !info.IsDir() {
			t.Fatalf("runtime directory %s missing: %v", got, err)
		}
	}
}
