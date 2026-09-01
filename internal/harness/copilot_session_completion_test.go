package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeSessionLog builds a minimal Copilot session log holding one
// assistant.message plus the shutdown event the converter needs to consider the
// log usable.
func writeSessionLog(t *testing.T, finalContent string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	lines := []map[string]any{
		{
			"type": "user.message",
			"data": map[string]any{"content": "", "transformedContent": "do the thing"},
		},
		{
			"type": "assistant.message",
			"data": map[string]any{
				"messageId":    "m1",
				"model":        "gpt-5.6-luna",
				"content":      finalContent,
				"toolRequests": []any{},
			},
		},
		{
			"type": "session.shutdown",
			"data": map[string]any{
				"shutdownType":         "routine",
				"totalNanoAiu":         1000,
				"totalPremiumRequests": 0,
				"modelMetrics": map[string]any{
					"gpt-5.6-luna": map[string]any{
						"requests": map[string]any{"count": 1, "cost": 0},
						"usage":    map[string]any{"inputTokens": 10, "outputTokens": 5},
					},
				},
			},
		},
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create session log: %v", err)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			t.Fatalf("encode session event: %v", err)
		}
	}
	return path
}

// The regression this exists for: Copilot writes a well-formed completion to its
// session log but echoes nothing to stdout under --silent --output-format=text
// with MCP tools attached. The harness must recover the completion from the log
// instead of failing the stage on an empty stdout capture.
func TestReadCopilotCompletionFromSessionRecoversEnvelope(t *testing.T) {
	completion := `{"status":"success","outputs":{"findingResponses":"[{\"finding\":1,\"disposition\":\"addressed\",\"detail\":\"Added rejection assertions.\"}]"},"summary":"Committed 858e8c1.","metrics":{"files_changed":1}}`
	path := writeSessionLog(t, completion)

	got, ok := readCopilotCompletionFromSession(ModeInvoke, path, 0)
	if !ok {
		t.Fatal("readCopilotCompletionFromSession did not recover the completion")
	}
	var env struct {
		Status  string            `json:"status"`
		Outputs map[string]string `json:"outputs"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("recovered payload is not valid JSON: %v (%s)", err, got)
	}
	if env.Status != "success" {
		t.Fatalf("recovered wrong envelope: %s", got)
	}
	if _, ok := env.Outputs["findingResponses"]; !ok {
		t.Fatalf("recovered envelope lost its outputs: %s", got)
	}
}

// A malformed final message must NOT be laundered into a valid completion: the
// fallback runs the same validation as the stdout path.
func TestReadCopilotCompletionFromSessionRejectsMalformed(t *testing.T) {
	path := writeSessionLog(t, "I finished the work but here is prose, not an envelope.")
	if _, ok := readCopilotCompletionFromSession(ModeInvoke, path, 0); ok {
		t.Fatal("recovered a completion from a message that carries none")
	}
}

// Valid JSON that is not a completion envelope must still be rejected.
func TestReadCopilotCompletionFromSessionRejectsNonEnvelopeJSON(t *testing.T) {
	path := writeSessionLog(t, `{"unrelated":"json","not":"an envelope"}`)
	if _, ok := readCopilotCompletionFromSession(ModeInvoke, path, 0); ok {
		t.Fatal("accepted valid JSON that is not a completion envelope")
	}
}

// A missing log is a clean miss, not a panic.
func TestReadCopilotCompletionFromSessionMissingPath(t *testing.T) {
	if _, ok := readCopilotCompletionFromSession(ModeInvoke, "", 0); ok {
		t.Fatal("empty path reported success")
	}
	if _, ok := readCopilotCompletionFromSession(ModeInvoke, filepath.Join(t.TempDir(), "nope.jsonl"), 0); ok {
		t.Fatal("missing file reported success")
	}
}
