package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/version"
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
	if mcpLogPath != "" {
		argv = append(argv, "--log-dir", mcpLogPath)
	}
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
	defer os.RemoveAll(dir)

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
	if confinement != nil {
		return confinement.logDir, func() {}, nil
	}
	if !req.GoobersIORegistered && len(req.MCPServers) == 0 {
		return "", func() {}, nil
	}
	dir, err := os.MkdirTemp(filepath.Join(req.Workspace, ".goobers"), "copilot-log-")
	if err != nil {
		return "", func() {}, fmt.Errorf("prepare MCP diagnostics: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
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
