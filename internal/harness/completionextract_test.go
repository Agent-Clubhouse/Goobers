package harness

import (
	"encoding/json"
	"testing"
)

// TestLastJSONValuePicksCompletionAfterToolNoise verifies that a tool-using
// model's captured final turn may contain JSON that is not the completion
// (shell output it echoed,
// a file it read back, a fragment it quoted while narrating). firstJSONValue
// must select the last well-formed value.
func TestLastJSONValuePicksCompletionAfterToolNoise(t *testing.T) {
	completion := `{"status":"success","outputs":{"findingResponses":"[{\"finding\":1,\"disposition\":\"addressed\",\"detail\":\"Expanded tests.\"}]"},"summary":"Committed 6fb4a6d.","metrics":{"files_changed":1}}`

	cases := []struct {
		name    string
		payload string
	}{
		{
			name: "prose preamble then completion",
			payload: "I'll confirm the committed branch state and then return the completion envelope only.\n" +
				completion,
		},
		{
			name: "leading tool-call JSON then completion",
			payload: `{"id":"call_abc","name":"bash","arguments":{"command":"git status"}}` + "\n" +
				completion,
		},
		{
			name: "shell output containing braces then completion",
			payload: "## goobers/default-implement/0dcaf58\n" +
				`{"content":"","startLine":1,"endLine":0,"totalLines":0}` + "\n" +
				"<shellId: 0 completed with exit code 0>\n" +
				completion,
		},
		{
			name: "json array from a read_input then completion",
			payload: `[{"severity":"error","message":"Add negative assertions."}]` + "\n" +
				completion,
		},
		{
			name:    "fenced completion after prose",
			payload: "Here is the result.\n```json\n" + completion + "\n```",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCompletionJSON([]byte(tc.payload))
			if !json.Valid(got) {
				t.Fatalf("extractCompletionJSON returned invalid JSON: %s", got)
			}
			var env struct {
				Status  string            `json:"status"`
				Outputs map[string]string `json:"outputs"`
			}
			if err := json.Unmarshal(got, &env); err != nil {
				t.Fatalf("unmarshal extracted payload: %v (payload=%s)", err, got)
			}
			if env.Status != "success" {
				t.Fatalf("extracted the wrong JSON value: status=%q, payload=%s", env.Status, got)
			}
			if _, ok := env.Outputs["findingResponses"]; !ok {
				t.Fatalf("extracted value is not the completion envelope: %s", got)
			}
		})
	}
}

// A bare completion with no surrounding noise must still round-trip untouched.
func TestExtractCompletionJSONBarePayloadUnchanged(t *testing.T) {
	completion := `{"status":"no-work","summary":"nothing to do"}`
	got := string(extractCompletionJSON([]byte("  " + completion + "  ")))
	if got != completion {
		t.Fatalf("bare completion was altered: got %q want %q", got, completion)
	}
}

// Braces inside string literals must not confuse the backwards scan.
func TestLastJSONValueHonorsStringLiterals(t *testing.T) {
	payload := `noise {"a":1}` + "\n" +
		`{"status":"success","summary":"contains a brace } and a bracket ] in prose"}`
	got := extractCompletionJSON([]byte(payload))
	var env struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("unmarshal: %v (payload=%s)", err, got)
	}
	if env.Status != "success" {
		t.Fatalf("picked wrong value: %s", got)
	}
}

// No JSON at all must report failure rather than returning a bogus span.
func TestLastJSONValueNoJSON(t *testing.T) {
	if _, ok := lastJSONValue([]byte("no json here at all")); ok {
		t.Fatal("lastJSONValue reported success on a payload with no JSON")
	}
}
