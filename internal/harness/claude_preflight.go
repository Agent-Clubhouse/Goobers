package harness

import (
	"context"
	"fmt"
	"strings"
)

func (c *ClaudeAdapter) preflightCredentialEnv(ctx context.Context, env []string) ([]string, error) {
	if c.ModelCredential == nil {
		return env, nil
	}
	token, err := c.ModelCredential(ctx)
	if err != nil {
		return nil, fmt.Errorf("harness: claude-code: resolve agent:model credential: %w", err)
	}
	// A shared, unscoped Copilot grant must retain the existing stored-login
	// fallback instead of becoming an invalid Anthropic credential.
	if !strings.HasPrefix(token, "sk-ant-") {
		return env, nil
	}
	env = withoutEnvVars(append([]string(nil), env...), "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN")
	return normalizeAnthropicCredentialEnv(overrideEnv(env, "ANTHROPIC_API_KEY", token)), nil
}
