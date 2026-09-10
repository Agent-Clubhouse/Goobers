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
	cleanup                   func()
}

func (c *CopilotAdapter) prepareCopilotCaptures(ctx context.Context, req RunRequest, argv, env []string) (copilotCapturePaths, error) {
	usageReq := req
	if c.DisableUsageOutput {
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
	return copilotCapturePaths{
		argv: argv, env: env, transcriptPath: transcriptPath, usagePath: usagePath,
		cleanup: func() { cleanupSession(); cleanupUsage() },
	}, nil
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
