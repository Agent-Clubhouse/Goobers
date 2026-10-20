package harness

import (
	"strings"
	"testing"
)

func TestInteractiveMCPOverridesUseRequiredBareHostServerName(t *testing.T) {
	overrides := appendCodexStdioOverrides(nil, "goobers-interactive", "interactive-prototype", []string{"agent-tools", "--socket", "/private/invocation/rpc.sock"}, nil, nil)
	joined := strings.Join(overrides, "\n")
	if strings.Contains(joined, `mcp_servers."goobers-interactive"`) || !strings.Contains(joined, `mcp_servers.goobers-interactive.required=true`) || !strings.Contains(joined, `mcp_servers.goobers-interactive.args=["agent-tools", "--socket", "/private/invocation/rpc.sock"]`) {
		t.Fatalf("invocation bridge cannot be materialized reliably: %s", joined)
	}
}
