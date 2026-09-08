package harness

import (
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

func prepareCopilotUsageOutput(req RunRequest, argv []string) ([]string, string, func()) {
	if !copilotSupportsUsageOutput(req.HarnessVersion) {
		// Unknown versions retain the same isolated native-session accounting as
		// older CLIs. No stale usage file may upgrade that fallback's cost basis.
		return argv, "", func() {}
	}
	relative := filepath.Join(".goobers", "copilot-usage.json")
	path := filepath.Join(req.Workspace, relative)
	_ = os.Remove(path)
	return append(argv, "--usage-output-file", relative), path, func() { _ = os.Remove(path) }
}
