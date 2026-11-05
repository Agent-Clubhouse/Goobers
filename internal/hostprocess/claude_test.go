package hostprocess

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeHookBlocksWrappedNameKills(t *testing.T) {
	for _, command := range []string{
		"pkill -f cat", "killall cat", "/custom/bin/pkill cat",
		"bash -c 'pkill -f cat'", `sh -c "killall cat"`, "eval pkill cat",
		"pgrep -f cat | xargs -I{} kill {}", "pgrep -f cat | xargs -I{} pkill {}",
		`find . -exec pkill cat \;`, "$(command -v pkill) cat", "cd /usr/bin && ./pkill cat",
		"kill $(pgrep -f cat)",
	} {
		t.Run(command, func(t *testing.T) {
			input, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
			var out, diagnostic bytes.Buffer
			if code := ClaudeHook(bytes.NewReader(input), &out, &diagnostic); code != 0 {
				t.Fatalf("hook code = %d", code)
			}
			var decision struct {
				Output struct {
					Event    string `json:"hookEventName"`
					Decision string `json:"permissionDecision"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(out.Bytes(), &decision); err != nil || decision.Output.Event != "PreToolUse" || decision.Output.Decision != "deny" {
				t.Fatalf("not a PreToolUse deny: %s (%v)", &out, err)
			}
			if strings.Contains(out.String()+diagnostic.String(), command) {
				t.Fatal("hook echoed potentially credential-bearing command")
			}
		})
	}
}

func TestClaudeHookDefersSafeCommandsToNormalPermissions(t *testing.T) {
	for _, command := range []string{"git status", "go test ./...", "kill -TERM 12345", "pgrep -f fixture", "echo pkill_count"} {
		input, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
		var out, diagnostic bytes.Buffer
		if code := ClaudeHook(bytes.NewReader(input), &out, &diagnostic); code != 0 || out.Len() != 0 || diagnostic.Len() != 0 {
			t.Fatalf("safe command %q emitted an overriding permission decision: code=%d, %s, %s", command, code, &out, &diagnostic)
		}
	}
}

func TestClaudeHookInvalidInputFailsClosed(t *testing.T) {
	for _, input := range []string{
		"", "{", "null", "{}", `{"tool_name":"Bash","tool_input":{"command":"git status"}}`,
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":42}}`,
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`,
		strings.Repeat("x", 1<<20+1),
	} {
		var out, diagnostic bytes.Buffer
		if code := ClaudeHook(strings.NewReader(input), &out, &diagnostic); code != 2 || out.Len() != 0 || !strings.Contains(diagnostic.String(), "command blocked") {
			t.Fatalf("malformed input did not block: code=%d, %s, %s", code, &out, &diagnostic)
		}
	}
}
