package codexprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeUnionsStayJSON(t *testing.T) {
	approval := AskForApproval(`"never"`)
	b, err := json.Marshal(TurnStartParams{ThreadId: "thread", ApprovalPolicy: &approval, Input: []UserInput{json.RawMessage(`{"type":"text","text":"hello","text_elements":[]}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["approvalPolicy"] != "never" {
		t.Fatalf("union encoded incorrectly: %s", b)
	}
	if strings.Contains(string(b), `"effort"`) {
		t.Fatalf("absent optional effort must stay omitted: %s", b)
	}
	input := got["input"].([]any)[0].(map[string]any)
	if input["text"] != "hello" {
		t.Fatal(input)
	}
}
