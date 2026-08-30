package workerhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
)

// TestScratchWorkspaceRemoveNamesTheLeakedPath covers #3645: teardown
// failures are no longer discarded by the engine, so the error a workspace
// returns has to say which working copy leaked — a bare "permission denied"
// gives an operator nothing to sweep.
func TestScratchWorkspaceRemoveNamesTheLeakedPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so removal cannot be made to fail")
	}
	scratchDir := filepath.Join(t.TempDir(), "scratch")
	p := &WorktreeWorkspaces{ScratchDir: scratchDir}
	ws, err := p.Provision(context.Background(), engine.WorkspaceRequest{Stage: "implement", Mode: apiv1.WorkspaceScratch})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// A read-only parent makes removing the workspace directory itself fail,
	// standing in for a locked or wedged working copy.
	if err := os.Chmod(scratchDir, 0o500); err != nil {
		t.Fatalf("chmod scratch root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(scratchDir, 0o700) })

	removeErr := ws.Remove(context.Background())
	if removeErr == nil {
		t.Fatal("Remove of an unremovable scratch workspace returned nil; the failure must surface")
	}
	if !strings.Contains(removeErr.Error(), ws.Path()) {
		t.Errorf("Remove error %q must name the leaked workspace %q", removeErr, ws.Path())
	}
	if !strings.Contains(removeErr.Error(), "workerhost") {
		t.Errorf("Remove error %q must identify the failing boundary", removeErr)
	}
}
