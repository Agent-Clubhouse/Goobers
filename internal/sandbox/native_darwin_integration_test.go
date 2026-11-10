//go:build integration && darwin

package sandbox

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// CLI startup needs no authentication or model call. Exercise the real
// installed launchers under the same profile used for stage execution.
func TestIntegrationNativeSandboxHarnessStartup(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_SANDBOX_HARNESS_STARTUP")
	testdep.Require(t, "copilot", "claude", "codex")
	s := requiredNativeSandbox(t)
	for _, name := range []string{"copilot", "claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			workspace := t.TempDir()
			command := exec.CommandContext(ctx, name, "--version")
			if err := s.Wrap(command, Policy{Workspace: workspace}); err != nil {
				t.Fatal(err)
			}
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("sandboxed %s startup: %v\n%s", name, err, output)
			}
		})
	}
}
