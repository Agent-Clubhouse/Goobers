package testdep

import (
	"os"
	"path/filepath"
)

// RequireSymlinks gates a test behind the environment's ability to create a
// symbolic link under dir. It is a capability gate in the same family as
// RequireUserNamespaces, not a tool gate: Windows grants symlink creation only
// to a privileged or developer-mode session, so a test reproducing a layout
// that contains one has no way to run there, and its failure would read as a
// product regression rather than an environment gap.
//
// The probe creates and removes one link inside dir, which the caller owns.
func RequireSymlinks(t TB, dir string) {
	t.Helper()
	probe := filepath.Join(dir, ".testdep-symlink-probe")
	if err := os.Symlink(dir, probe); err != nil {
		t.Skipf("integration test skipped: this environment cannot create symbolic links: %v", err)
		return
	}
	if err := os.Remove(probe); err != nil {
		t.Fatalf("remove symlink probe: %v", err)
	}
}
