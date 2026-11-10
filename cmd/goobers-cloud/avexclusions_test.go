package main

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/workerhost"
)

func TestWorkerAVExclusionDirectoriesFollowTheProvisioner(t *testing.T) {
	workRoot := t.TempDir()
	runtimeDeps, err := workerEngineDepsForPlatform(workRoot, "linux", "test-owner")
	if err != nil {
		t.Fatalf("provision worker engine deps: %v", err)
	}
	t.Cleanup(func() { _ = runtimeDeps.Close() })

	provisioned, ok := runtimeDeps.deps.Workspaces.(*workerhost.WorktreeWorkspaces)
	if !ok {
		t.Fatalf("worker workspaces = %T, want *workerhost.WorktreeWorkspaces", runtimeDeps.deps.Workspaces)
	}
	tempDir := filepath.Join(workRoot, "tmp")
	dirs := runtimewiring.WorkerAVExclusionDirectories(workRoot, runtimewiring.AvExclusionDeps{TempDir: tempDir})
	paths := map[string]bool{}
	for _, d := range dirs {
		paths[d.Path] = true
	}
	// worktree.NewManager absolutises its root, so compare like for like.
	wantWorkcopies, err := filepath.Abs(runtimewiring.WorkerWorkcopiesDir(workRoot))
	if err != nil {
		t.Fatal(err)
	}
	if provisioned.Manager.Root != wantWorkcopies {
		t.Fatalf("provisioner mirror root = %q, want %q", provisioned.Manager.Root, wantWorkcopies)
	}
	for _, want := range []string{workRoot, provisioned.Manager.Root, provisioned.ScratchDir, tempDir} {
		if !paths[want] {
			t.Errorf("worker advisory set %v does not name the provisioned directory %s", dirs, want)
		}
	}
}
