//go:build integration && darwin

package sandbox

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// CLI startup needs no authentication or model call. Exercise the real
// installed launchers under the same profile used for stage execution.
func TestNativeSandboxHarnessStartup(t *testing.T) {
	s := requiredNativeSandbox(t)
	for _, name := range []string{"copilot", "claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			path, err := exec.LookPath(name)
			if err != nil {
				t.Skipf("%s is not installed", name)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			workspace := t.TempDir()
			command := exec.CommandContext(ctx, path, "--version")
			if err := s.Wrap(command, Policy{Workspace: workspace}); err != nil {
				t.Fatal(err)
			}
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("sandboxed %s startup: %v\n%s", name, err, output)
			}
		})
	}
}
