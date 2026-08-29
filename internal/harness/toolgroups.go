package harness

import (
	"fmt"
	"strings"
)

// expandToolGroup expands a single harness-neutral tool declaration (e.g.
// "shell") against an adapter's own group table. A declaration that does not
// name a known group is returned unexpanded, so adapters can pass through
// tool names the vocabulary doesn't cover a group for.
func expandToolGroup(declared string, groups map[string][]string) []string {
	if group, ok := groups[strings.ToLower(declared)]; ok {
		return group
	}
	return []string{declared}
}

// validateToolAllowlist rejects allowlist entries that cannot survive being
// comma-joined into a single CLI flag value.
func validateToolAllowlist(adapterName string, tools []string) error {
	for i, tool := range tools {
		if strings.Contains(tool, ",") {
			return fmt.Errorf("harness: %s: tool allowlist entry %d %q must not contain a comma", adapterName, i, tool)
		}
	}
	return nil
}
