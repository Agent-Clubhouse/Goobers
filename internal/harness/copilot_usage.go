package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/version"

	"github.com/goobers/goobers/internal/diagnostictmp"
)

// The per-agent usage-file schema is documented in the upstream 1.0.81-1
// release: https://github.com/github/copilot-cli/releases/tag/v1.0.81-1.
// Copilot 1.0.80 rejects the flag before doing any agent work. Use the version
// already captured by preflight; never probe by starting or replaying a stage.
var copilotUsageVersion = regexp.MustCompile(`^(?:GitHub Copilot CLI |copilot version |v?)([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)\.?(?:\s|$)`)
var copilotUsageMinimum = version.MustParseSemantic("1.0.81-1")

func copilotSupportsUsageOutput(reported string) bool {
	match := copilotUsageVersion.FindStringSubmatch(strings.TrimSpace(reported))
	if len(match) != 2 {
		return false
	}
	parsed, err := version.ParseSemantic(strings.TrimRight(match[1], "."))
	return err == nil && !parsed.LessThan(copilotUsageMinimum)
}

func copilotUsageCapture(reported string) string {
	if copilotSupportsUsageOutput(reported) {
		return "usage-file-with-session-fallback"
	}
	return "session-transcript"
}

// copilotCapturePaths owns both captures for one invocation, so a failed
// launcher handshake also releases any usage directory prepared before it.
type copilotCapturePaths struct {
	argv, env                 []string
	transcriptPath, usagePath string
	mcpLogPath                string
	cleanup                   func()
}

func (c *CopilotAdapter) prepareCopilotCaptures(ctx context.Context, req RunRequest, argv, env []string, confinement *copilotConfinement) (copilotCapturePaths, error) {
	for _, arg := range argv {
		if arg == "--log-dir" || strings.HasPrefix(arg, "--log-dir=") {
			return copilotCapturePaths{}, fmt.Errorf("copilot log directory is adapter-managed; remove --log-dir from harness arguments")
		}
	}
	usageReq := req
	if c.usageOutputDisabled() {
		usageReq.HarnessVersion = ""
	}
	argv, usagePath, cleanupUsage, err := prepareCopilotUsageOutput(usageReq, argv)
	if err != nil {
		return copilotCapturePaths{}, err
	}
	argv, env, transcriptPath, cleanupSession, err := c.prepareLauncherSession(ctx, req.Workspace, argv, env)
	if err != nil {
		cleanupUsage()
		return copilotCapturePaths{}, err
	}
	mcpLogPath, cleanupMCPLog, err := prepareCopilotMCPLog(req, confinement)
	if err != nil {
		cleanupSession()
		cleanupUsage()
		return copilotCapturePaths{}, err
	}
	argv = append(argv, "--log-dir", mcpLogPath)
	return copilotCapturePaths{
		argv: argv, env: env, transcriptPath: transcriptPath, usagePath: usagePath, mcpLogPath: mcpLogPath,
		cleanup: func() { cleanupMCPLog(); cleanupSession(); cleanupUsage() },
	}, nil
}

func (c *CopilotAdapter) usageOutputDisabled() bool {
	if !c.DisableUsageOutput {
		return false
	}
	c.launcherMu.Lock()
	defer c.launcherMu.Unlock()
	return !c.launcherUsageVerified
}

func (c *CopilotAdapter) verifyLauncherUsageOutput(ctx context.Context, version string, versionArgs []string) {
	if !c.DisableUsageOutput || !copilotSupportsUsageOutput(version) {
		return
	}
	dir, err := os.MkdirTemp("", "goobers-copilot-usage-probe-")
	if err != nil {
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()

	stdout := newTranscriptBuffer(maxPreflightDiagnosticBytes)
	command := append(append([]string(nil), resolveHarnessCommand(c.Command)...),
		"--usage-output-file", filepath.Join(dir, "usage.json"))
	command = append(command, versionArgs...)
	result, err := c.runner().Run(ctx, ProcessRequest{
		Command:            command,
		Env:                baseEnv(c.ExtraEnvAllowlist, c.EnvUnset),
		MaxTranscriptBytes: maxPreflightDiagnosticBytes,
		StdoutCapture:      stdout,
	})
	if err != nil || result.ExitCode != 0 || stdout.Truncated() || !copilotSupportsUsageOutput(firstOutputLine(stdout.Bytes())) {
		return
	}
	c.launcherMu.Lock()
	c.launcherUsageVerified = true
	c.launcherMu.Unlock()
}

func prepareCopilotMCPLog(req RunRequest, confinement *copilotConfinement) (string, func(), error) {
	// The CLI can log its complete environment, including credentials. Keep
	// every invocation's raw logs outside the checkout, even without MCP,
	// and discard them after extracting structured diagnostics.
	dir, cleanup, err := copilotPrivateTempDir(req.Workspace)
	if err != nil {
		return "", func() {}, fmt.Errorf("prepare MCP diagnostics: %w", err)
	}
	if confinement != nil {
		// Grant only this invocation's private directory, never the temp root.
		confinement.writableRoots = append(confinement.writableRoots, dir)
	}
	return dir, cleanup, nil
}

// copilotPrivateTempDir leases its directory so sweeping abandoned diagnostics
// cannot remove another process's active capture in the same system temp root.
func copilotPrivateTempDir(workspace string) (string, func(), error) {
	parent, err := copilotDiagnosticTempRoot(workspace)
	if err != nil {
		return "", nil, err
	}
	scope, err := diagnostictmp.Establish(parent)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = scope.Reclaim() }
	return scope.Dir(), cleanup, nil
}

// copilotDiagnosticTempRoot refuses a TMPDIR (including a symlink alias) that
// points back into the checkout. A private temp name alone does not keep raw
// diagnostic data out of a repository when the daemon's temp root is local.
func copilotDiagnosticTempRoot(workspace string) (string, error) {
	canonical := func(path string) (string, error) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(absolute)
	}
	root, err := canonical(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve diagnostic workspace: %w", err)
	}
	parent, err := canonical(os.TempDir())
	if err != nil {
		return "", fmt.Errorf("resolve diagnostic temp directory: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("inspect diagnostic workspace: %w", err)
	}
	// SameFile also handles case aliases on case-insensitive filesystems and
	// workspaces on a different volume from the system temp directory.
	for ancestor := parent; ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Stat(ancestor)
		if err != nil {
			return "", fmt.Errorf("inspect diagnostic temp ancestor: %w", err)
		}
		if os.SameFile(rootInfo, info) {
			return "", fmt.Errorf("diagnostic temp directory must be outside the workspace")
		}
		if filepath.Dir(ancestor) == ancestor {
			break
		}
	}
	return parent, nil
}

func prepareCopilotUsageOutput(req RunRequest, argv []string) ([]string, string, func(), error) {
	if !copilotSupportsUsageOutput(req.HarnessVersion) {
		// Unknown versions retain the same isolated native-session accounting as
		// older CLIs. No stale usage file may upgrade that fallback's cost basis.
		return argv, "", func() {}, nil
	}
	// The prompt has already established .goobers as writable. Keep capture
	// inside it (and therefore inside the sandbox's workspace grant), but use
	// a new private directory for EVERY invocation. Cleanup can fail or the
	// host can crash: neither makes an old document eligible for the next run.
	dir, err := os.MkdirTemp(filepath.Join(req.Workspace, ".goobers"), "copilot-usage-")
	if err != nil {
		return nil, "", nil, fmt.Errorf("prepare fresh Copilot usage capture: %w", err)
	}
	relative := filepath.Join(".goobers", filepath.Base(dir), "usage.json")
	path := filepath.Join(dir, "usage.json")
	return append(argv, "--usage-output-file", relative), path, func() { _ = os.RemoveAll(dir) }, nil
}
