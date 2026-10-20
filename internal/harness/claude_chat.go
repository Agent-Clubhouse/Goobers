package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/chat"
	claudechat "github.com/goobers/goobers/internal/chat/claude"
)

// ChatDriver binds a native Claude session to the same trusted request, scoped
// credentials, tool restrictions and sandbox as the batch adapter. The caller
// must retain exclusive workspace custody until Session.Close succeeds.
func (c *ClaudeAdapter) ChatDriver(req RunRequest) *claudechat.Driver {
	return &claudechat.Driver{
		ProbeOptions: chat.StartOptions{Workspace: req.Workspace, Level: chat.Plan},
		Prepare: func(ctx context.Context, o chat.StartOptions, args []string) (claudechat.Launch, error) {
			return c.prepareClaudeChat(ctx, req, o, args)
		},
	}
}
func (c *ClaudeAdapter) prepareClaudeChat(ctx context.Context, req RunRequest, o chat.StartOptions, args []string) (launch claudechat.Launch, err error) {
	if err := c.validateClaudeChatRequest(req, o); err != nil {
		return launch, err
	}
	options, err := normalizeClaudeConfig(req.Model, req.HarnessOptions)
	if err != nil {
		return launch, err
	}
	ephemeralTmp, err := establishEphemeralTmp(c.Name(), c.EphemeralTmp, c.EphemeralTmpRoot)
	if err != nil {
		return launch, err
	}
	defer func() {
		if err != nil {
			_ = ephemeralTmp.Reclaim()
		}
	}()
	req = withAutoGoobersIOClaude(req, c.SelfBin)
	var configs []string
	ioConfig, err := goobersIOClaudeMCPConfigArg(req, c.SelfBin)
	if err != nil {
		return launch, err
	}
	if ioConfig != "" {
		configs = append(configs, ioConfig)
	}
	declared, mcpEnv, err := prepareClaudeMCP(ctx, req)
	if err != nil {
		return launch, err
	}
	if declared != "" {
		configs = append(configs, declared)
	}
	argv := append(resolveStdioHarnessCommand(c.Command), args...)
	// Strict even when no servers are declared: ambient project registrations
	// must not become authority for an interactive session.
	argv = append(argv, "--strict-mcp-config")
	if len(configs) > 0 {
		argv = append(argv, "--mcp-config")
		argv = append(argv, configs...)
	}
	argv = append(argv, claudeHostSafetyArgs()...)
	if len(req.Tools) > 0 {
		argv = append(argv, "--tools", strings.Join(claudeAvailableTools(req.Tools), ","))
	}
	if o.Model == "" && req.Model != "" && req.Model != "auto" {
		argv = append(argv, "--model", req.Model)
	}
	if options["effort"] != "" {
		argv = append(argv, "--effort", options["effort"])
	}
	env, err := buildCredentialEnv(ctx, credentialEnvConfig{
		adapterName: c.Name(), envCapabilities: c.EnvCapabilities, optionalCredentialCapabilities: c.OptionalCredentialCapabilities,
		extraEnvAllowlist: c.ExtraEnvAllowlist, envUnset: c.EnvUnset, instanceRoot: c.InstanceRoot, selfBin: c.SelfBin, ephemeralTmp: ephemeralTmp,
	}, req)
	if err != nil {
		return launch, err
	}
	env = normalizeAnthropicCredentialEnv(append(env, mcpEnv...))
	env, err = c.prepareInteractiveRuntimeEnv(ctx, req, env)
	if err != nil {
		return launch, err
	}
	if req.Sandbox != nil {
		roots, err := gitWritableRoots(req.Workspace)
		if err != nil {
			return launch, err
		}
		wrapped, _, err := confineArgv(req.Sandbox, argv, req.Workspace, roots)
		if err != nil {
			return launch, fmt.Errorf("claude chat: sandbox: %w", err)
		}
		argv = wrapped
	}
	return claudechat.Launch{Command: argv, Env: env, Dir: req.Workspace, Cleanup: ephemeralTmp.Reclaim}, nil
}

func (c *ClaudeAdapter) validateClaudeChatRequest(req RunRequest, o chat.StartOptions) error {
	if err := validateStandardExecution(req); err != nil {
		return err
	}
	if len(c.Command) == 0 || req.Workspace == "" {
		return errors.New("claude chat: command and workspace required")
	}
	if o.Workspace != "" && o.Workspace != req.Workspace {
		return errors.New("claude chat: workspace differs from authorized request")
	}
	if len(c.ExtraArgs) > 0 {
		return errors.New("claude chat: extra argv is unsupported by native transport")
	}
	if err := validateToolAllowlist(c.Name(), req.Tools); err != nil {
		return err
	}
	return nil
}
