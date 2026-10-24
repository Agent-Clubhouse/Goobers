//go:build integration

package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This opt-in check asks only auth.getStatus: it never creates a session or
// sends a prompt. It verifies the stored-login compatibility preflight against
// the installed CLI without importing login files into a private home.
func TestIntegrationCopilotStoredLoginPreflightWithoutCopy(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_COPILOT_STORED_AUTH")
	testdep.Require(t, "copilot")
	env := withoutEnvVars(baseEnv([]string{"COPILOT_HOME"}, nil), "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN")
	original, ok := copilotConfigHome(env)
	if !ok {
		t.Fatal("no stored-login home available")
	}
	prepared, work, cleanup, err := prepareCopilotPreflightEnvironment(env, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if home, _ := copilotConfigHome(prepared); home != original {
		t.Fatal("stored-login home replaced")
	}
	if _, err := os.Stat(filepath.Join(work, copilotConfigFileName)); !os.IsNotExist(err) {
		t.Fatal("login file copied into probe directory")
	}
	client := copilot.NewClient(&copilot.ClientOptions{Connection: copilot.StdioConnection{Path: "copilot", Args: []string{"--no-auto-update"}, Env: prepared}, WorkingDirectory: work, LogLevel: "none"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal("Copilot auth transport did not start")
	}
	defer func() {
		if err := client.Stop(); err != nil {
			t.Error("Copilot auth transport did not stop cleanly")
		}
	}()
	status, err := client.GetAuthStatus(ctx)
	if err != nil {
		t.Fatal("read-only auth status failed")
	}
	if status == nil || !status.IsAuthenticated {
		t.Fatal("stored CLI login was not preserved")
	}
}
