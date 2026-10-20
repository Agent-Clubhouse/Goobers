package harness

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/goobers/goobers/internal/invoke"
)

// InteractiveCopilotProcess owns an authenticated SDK connection to a process
// launched with the adapter's environment, confinement and process-tree runner.
// The caller owns SDK sessions and must disconnect them when closing.
type InteractiveCopilotProcess struct {
	Client         *copilot.Client
	Home           string
	AvailableTools []string
	Servers        map[string]copilot.MCPServerConfig
	Permissions    copilot.PermissionHandlerFunc
	process        *copilotControlProcess
	proof          *invoke.WorkspaceQuiescence
	cleanup        func() error
	once           sync.Once
	closeErr       error
}

func (p *InteractiveCopilotProcess) Close() error {
	p.once.Do(func() {
		p.process.close()
		p.closeErr = p.proof.Verify()
		if p.closeErr == nil {
			p.closeErr = p.cleanup()
		}
	})
	return p.closeErr
}

// OpenInteractive reuses the adapter's host policy and owned process machinery.
// Unlike prompt-mode controls it deliberately omits --no-auto-update: that flag
// can select a shim's bundled binary instead of the installed CLI (Clubhouse #2154).
func (c *CopilotAdapter) OpenInteractive(ctx context.Context, req RunRequest) (result *InteractiveCopilotProcess, err error) {
	if len(c.Command) == 0 || req.Workspace == "" {
		return nil, fmt.Errorf("interactive Copilot requires command and workspace")
	}
	if err := validateStandardExecution(req); err != nil {
		return nil, err
	}
	req.Tools = appendRequiredTools(req.Tools, c.RequiredTools)
	if err := validateCopilotTools(req.Tools); err != nil {
		return nil, err
	}
	scope, err := establishEphemeralTmp(c.Name(), c.EphemeralTmp, c.EphemeralTmpRoot)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			err = errors.Join(err, scope.Reclaim())
		}
	}()
	env, err := c.credentialEnv(ctx, scope, req)
	if err != nil {
		return nil, err
	}
	var roots []string
	if req.Sandbox != nil {
		conf, err := prepareCopilotConfinement(req.Workspace)
		if err != nil {
			return nil, err
		}
		env = overrideEnv(env, "COPILOT_HOME", conf.copilotHome)
		env = overrideEnv(env, "TMPDIR", conf.tempDir)
		roots = conf.writableRoots
	}
	if len(req.MCPServers) > 0 {
		env, err = prepareCopilotMCP(ctx, req, env)
		if err != nil {
			return nil, err
		}
	}
	servers := map[string]copilot.MCPServerConfig{}
	if len(req.MCPServers) > 0 {
		home, ok := copilotConfigHome(env)
		if !ok {
			return nil, fmt.Errorf("interactive MCP home unavailable")
		}
		servers, err = copilotControlledServers(RunRequest{Workspace: req.Workspace}, env, filepath.Join(home, "mcp-config.json"))
		if err != nil {
			return nil, err
		}
	}
	command := append(resolveStdioHarnessCommand(append([]string(nil), c.Command...)), "--headless", "--port", "0", "--disable-builtin-mcps")
	command = withCopilotNoRemoteExport(command)
	if req.Sandbox != nil {
		command, _, err = confineArgv(req.Sandbox, command, req.Workspace, roots)
		if err != nil {
			return nil, err
		}
	}
	runCtx, proof := invoke.WithWorkspaceQuiescence(ctx)
	p, err := startCopilotControlServer(runCtx, c.runner(), ProcessRequest{Dir: req.Workspace, Env: env, Timeout: req.Timeout}, command)
	if err != nil {
		if proof.HasRegisteredWriters() {
			err = errors.Join(err, proof.Verify())
		}
		return nil, err
	}
	home, _ := copilotConfigHome(env)
	if home == "" {
		for _, entry := range env {
			if len(entry) > 5 && entry[:5] == "HOME=" {
				home = filepath.Join(entry[5:], ".copilot")
			}
		}
	}
	// No ambient HOME fallback: only the environment actually passed to the CLI
	// determines where resume may look.
	if home != "" {
		home, _ = filepath.Abs(home)
	}
	return &InteractiveCopilotProcess{Client: p.client, Home: home, AvailableTools: copilotAvailableTools(req), Servers: servers, Permissions: copilotSessionPermissions(req, roots), process: p, proof: proof, cleanup: scope.Reclaim}, nil
}
