// Package hostprocess provides defense-in-depth command checks. OS signal or
// PID confinement remains the boundary against arbitrary process signaling.
package hostprocess

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var nameKill = regexp.MustCompile(`\b(pkill|killall)\b`)
var processLookup = regexp.MustCompile(`\bpgrep\b`)
var signalCommand = regexp.MustCompile(`\bkill\b`)

const denial = "Goobers host-process safety: name-based process kills are blocked; only signal PIDs you started."

// ClaudeHook reads one PreToolUse event. Safe commands emit no decision, so
// normal permission policy still applies. Invalid input fails closed without
// reflecting command text, which can contain credentials.
func ClaudeHook(input io.Reader, stdout, stderr io.Writer) int {
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	var event struct {
		Event string `json:"hook_event_name"`
		Tool  string `json:"tool_name"`
		Input struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err != nil || len(data) > limit || json.Unmarshal(data, &event) != nil ||
		event.Event != "PreToolUse" || event.Tool != "Bash" || strings.TrimSpace(event.Input.Command) == "" {
		_, _ = fmt.Fprintln(stderr, "Goobers host-process safety: cannot inspect Bash hook input; command blocked.")
		return 2
	}
	command := event.Input.Command
	if nameKill.MatchString(command) || (processLookup.MatchString(command) && signalCommand.MatchString(command)) {
		output := map[string]any{"hookSpecificOutput": map[string]string{
			"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": denial,
		}}
		if err := json.NewEncoder(stdout).Encode(output); err != nil {
			return 2
		}
	}
	return 0
}
