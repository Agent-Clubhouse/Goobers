//go:build integration

package harness

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationCodexAmbientChatGPTSmoke is intentionally opt-in: it consumes
// the operator's ChatGPT subscription limits and never runs in normal CI.
func TestIntegrationCodexAmbientChatGPTSmoke(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CODEX_AMBIENT_SMOKE")

	cmd := exec.Command("codex", "exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check", "Respond with exactly: subscription-auth-ok")
	cmd.Env = withoutEnvVars(os.Environ(), codexModelEnv)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ambient Codex smoke failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "subscription-auth-ok") {
		t.Fatalf("ambient Codex smoke output did not contain expected response: %s", output)
	}
}
