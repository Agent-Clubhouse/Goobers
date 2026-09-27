package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	got, why, ok := readCopilotCompletionFromSession(ModeInvoke, path, 0)
	if !ok {
		t.Fatalf("readCopilotCompletionFromSession did not recover the completion: %s", why)
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
	if _, _, ok := readCopilotCompletionFromSession(ModeInvoke, path, 0); ok {
		t.Fatal("recovered a completion from a message that carries none")
	}
}

// Valid JSON that is not a completion envelope must still be rejected.
func TestReadCopilotCompletionFromSessionRejectsNonEnvelopeJSON(t *testing.T) {
	path := writeSessionLog(t, `{"unrelated":"json","not":"an envelope"}`)
	if _, _, ok := readCopilotCompletionFromSession(ModeInvoke, path, 0); ok {
		t.Fatal("accepted valid JSON that is not a completion envelope")
	}
}

// A missing log is a clean miss, not a panic.
func TestReadCopilotCompletionFromSessionMissingPath(t *testing.T) {
	if _, _, ok := readCopilotCompletionFromSession(ModeInvoke, "", 0); ok {
		t.Fatal("empty path reported success")
	}
	if _, _, ok := readCopilotCompletionFromSession(ModeInvoke, filepath.Join(t.TempDir(), "nope.jsonl"), 0); ok {
		t.Fatal("missing file reported success")
	}
}

// sessionEvent names one event in a synthetic session log: a user prompt
// ("user") or an assistant message ("assistant").
type sessionEvent struct {
	role    string
	content string
}

// writeSessionLogEvents builds a session log from an explicit event sequence so
// a test can reproduce a multi-turn log: the well-formed completion, the repair
// prompt that follows it, and whatever trailing message masked it.
func writeSessionLogEvents(t *testing.T, events ...sessionEvent) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	lines := make([]map[string]any, 0, len(events)+1)
	for i, event := range events {
		switch event.role {
		case "user":
			lines = append(lines, map[string]any{
				"type": "user.message",
				"data": map[string]any{"content": "", "transformedContent": event.content},
			})
		case "assistant":
			lines = append(lines, map[string]any{
				"type": "assistant.message",
				"data": map[string]any{
					"messageId": fmt.Sprintf("m%d", i),
					"model":     "gpt-5.6-luna",
					"content":   event.content,
				},
			})
		default:
			t.Fatalf("unknown role %q", event.role)
		}
	}
	lines = append(lines, map[string]any{
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
	})

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

const sessionTestCompletion = `{"status":"success","outputs":{"findingResponses":"[{\"finding\":1,\"disposition\":\"addressed\",\"detail\":\"Hardened the helper.\"}]"},"summary":"Committed the fix.","metrics":{"files_changed":1}}`

func assertRecoversCompletion(t *testing.T, path, wantSummary string) {
	t.Helper()
	got, why, ok := readCopilotCompletionFromSession(ModeInvoke, path, 0)
	if !ok {
		t.Fatalf("did not recover the completion: %s", why)
	}
	var env struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("recovered payload is not valid JSON: %v (%s)", err, got)
	}
	if env.Status != "success" {
		t.Fatalf("recovered wrong envelope: %s", got)
	}
	if wantSummary != "" && env.Summary != wantSummary {
		t.Fatalf("recovered the wrong turn's completion: summary=%q want %q", env.Summary, wantSummary)
	}
}

// #4752: the turn does not always end ON its completion. Copilot appends a
// trailing empty assistant.message after the well-formed envelope, and keeping
// only the literal last message made recovery report "no completion" for a
// completion sitting one event earlier -- failing the stage and discarding work
// the agent had already committed to the branch.
func TestReadCopilotCompletionFromSessionSkipsTrailingEmptyMessage(t *testing.T) {
	path := writeSessionLogEvents(t,
		sessionEvent{"user", "do the thing"},
		sessionEvent{"assistant", sessionTestCompletion},
		sessionEvent{"assistant", ""},
	)
	assertRecoversCompletion(t, path, "Committed the fix.")
}

// Same defect, prose variant: a plain sign-off after the envelope.
func TestReadCopilotCompletionFromSessionSkipsTrailingProseMessage(t *testing.T) {
	path := writeSessionLogEvents(t,
		sessionEvent{"user", "do the thing"},
		sessionEvent{"assistant", sessionTestCompletion},
		sessionEvent{"assistant", "All done - let me know if you need anything else."},
	)
	assertRecoversCompletion(t, path, "Committed the fix.")
}

// Walking back must stop at the turn boundary. A completion the model was
// afterwards asked to supersede (the repair prompt is a new user.message) must
// never be recovered in place of the current turn's answer -- that would report
// success for work the latest turn did not actually signal.
func TestReadCopilotCompletionFromSessionDoesNotCrossTurnBoundary(t *testing.T) {
	path := writeSessionLogEvents(t,
		sessionEvent{"user", "do the thing"},
		sessionEvent{"assistant", sessionTestCompletion},
		sessionEvent{"user", "Your previous turn ended without returning the mandatory completion as valid JSON."},
		sessionEvent{"assistant", "I could not complete the task."},
	)
	got, why, ok := readCopilotCompletionFromSession(ModeInvoke, path, 0)
	if ok {
		t.Fatalf("recovered a superseded completion from an earlier turn: %s", got)
	}
	if why == "" {
		t.Fatal("failed recovery reported no reason")
	}
}

// Within one turn the NEWEST valid envelope wins, not the first.
func TestReadCopilotCompletionFromSessionPrefersNewestEnvelope(t *testing.T) {
	stale := `{"status":"success","outputs":{},"summary":"Stale.","metrics":{"files_changed":1}}`
	fresh := `{"status":"success","outputs":{},"summary":"Fresh.","metrics":{"files_changed":1}}`
	path := writeSessionLogEvents(t,
		sessionEvent{"user", "do the thing"},
		sessionEvent{"assistant", stale},
		sessionEvent{"assistant", fresh},
		sessionEvent{"assistant", ""},
	)
	assertRecoversCompletion(t, path, "Fresh.")
}

// A turn that produced no envelope at all still fails, and says why.
func TestReadCopilotCompletionFromSessionReportsWhyItFailed(t *testing.T) {
	path := writeSessionLogEvents(t,
		sessionEvent{"user", "do the thing"},
		sessionEvent{"assistant", "Here is some prose."},
		sessionEvent{"assistant", "And a sign-off."},
	)
	_, why, ok := readCopilotCompletionFromSession(ModeInvoke, path, 0)
	if ok {
		t.Fatal("recovered a completion from a turn that produced none")
	}
	if !strings.Contains(why, "2 assistant message") {
		t.Fatalf("reason does not report what was searched: %q", why)
	}
}

// The retained-candidate window is bounded, and a completion inside it is still
// found when a long tail of trailing chatter follows.
func TestReadCopilotCompletionFromSessionBoundsCandidates(t *testing.T) {
	events := []sessionEvent{
		{"user", "do the thing"},
		{"assistant", sessionTestCompletion},
	}
	for i := 0; i < maxCopilotCompletionCandidates-1; i++ {
		events = append(events, sessionEvent{"assistant", fmt.Sprintf("trailing chatter %d", i)})
	}
	assertRecoversCompletion(t, writeSessionLogEvents(t, events...), "Committed the fix.")

	// One more trailing message pushes the envelope out of the window: the
	// stage fails closed rather than scanning an unbounded history.
	events = append(events, sessionEvent{"assistant", "one too many"})
	if _, _, ok := readCopilotCompletionFromSession(ModeInvoke, writeSessionLogEvents(t, events...), 0); ok {
		t.Fatal("recovered a completion from beyond the retained candidate window")
	}
}
