package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

const claudeHostSafetyMinVersion = "2.1.302"
const claudeHostSafetyWarning = "host-process safety hook unavailable on this Claude Code version; retaining --disallowedTools name-kill denies. Upgrade to Claude Code " + claudeHostSafetyMinVersion + " or newer for the PreToolUse hook; use OS sandbox confinement for host isolation."

var claudeHostSafetyVersion = regexp.MustCompile(`^(\d+\.\d+\.\d+)(?:\s|$)`)

func validateClaudeHostSafetyVersion(version string) error {
	match := claudeHostSafetyVersion.FindStringSubmatch(version)
	if len(match) != 2 || semver.Compare("v"+match[1], "v"+claudeHostSafetyMinVersion) < 0 {
		return fmt.Errorf("harness: claude-code: host-process safety requires Claude Code %s or newer (exec-form hooks and fail-closed hook errors); upgrade the CLI", claudeHostSafetyMinVersion)
	}
	return nil
}

// User settings are isolated; project/local settings must not disable the
// hook. Explicit inline settings may add permissions, but cannot change hooks
// or inject environment variables. File settings are refused because their
// contents could change between inspection and the CLI reading them.
func validateClaudeHostSafetyOptions(args []string) error {
	if err := validateClaudeHostSafetyEnv(args); err != nil {
		return err
	}
	for i := 0; i < len(args); i++ {
		arg, value, equals := strings.Cut(args[i], "=")
		switch arg {
		case "--bare", "--safe-mode", "--restricted":
			return fmt.Errorf("harness: claude-code: %s can bypass host-process safety hooks; remove it for unattended stages", arg)
		case "--setting-sources", "--settings":
			if !equals {
				i++
				if i >= len(args) {
					return fmt.Errorf("harness: claude-code: missing %s value", arg)
				}
				value = args[i]
			}
			if arg == "--setting-sources" {
				if value != "user" {
					return fmt.Errorf("harness: claude-code: host-process safety requires --setting-sources user; project/local settings can disable the isolated hook")
				}
				continue
			}
			var settings map[string]json.RawMessage
			if json.Unmarshal([]byte(value), &settings) != nil || settings == nil {
				return fmt.Errorf("harness: claude-code: host-process safety requires inline JSON --settings; custom settings files are not accepted for unattended stages")
			}
			for _, key := range []string{"hooks", "disableAllHooks", "allowManagedHooksOnly", "env"} {
				if _, present := settings[key]; present {
					return fmt.Errorf("harness: claude-code: --settings key %s can override host-process safety; remove it for unattended stages", key)
				}
			}
		}
	}
	return nil
}

func validateClaudeHostSafetyEnv(env []string) error {
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		for _, blocked := range []string{"CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_SAFE_MODE", "CLAUDE_CODE_DISABLE_HOOKS"} {
			if strings.EqualFold(key, blocked) && value != "" && value != "0" && value != "false" {
				return fmt.Errorf("harness: claude-code: %s can disable host-process safety hooks; remove it for unattended stages", key)
			}
		}
	}
	return nil
}

func writeClaudeHostSafetySettings(configDir, selfBin string) error {
	var err error
	if selfBin == "" {
		selfBin, err = os.Executable()
		if err != nil {
			return err
		}
	}
	selfBin, err = filepath.Abs(selfBin)
	if err != nil {
		return err
	}
	settings := map[string]any{
		"permissions": map[string]any{"deny": claudeDeniedNameKillRules},
		"hooks": map[string]any{"PreToolUse": []any{
			map[string]any{"matcher": "Bash", "hooks": []any{
				map[string]any{"type": "command", "command": selfBin,
					"args": []string{"__claude-host-safety"}, "timeout": 10, "onFailure": "block"},
			}},
		}},
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(configDir, "settings.json"), data, 0600)
}
