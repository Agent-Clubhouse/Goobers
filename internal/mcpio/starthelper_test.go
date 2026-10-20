package mcpio

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract/childworkflowwire"
)

func TestStartHelperProtocolAndBounds(t *testing.T) {
	server, _ := newChildServer(t)
	calls := 0
	server.tools.childTransport = childRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/api/v1/runs/run-123/child-workflows/start" ||
			request.Header.Get("Idempotency-Key") != "inspect" || request.Header.Get("Authorization") != "Bearer "+childMCPGrant {
			t.Fatal("helper lost trusted scope or identity")
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body childworkflowwire.ChildWorkflowHelperRequest
		if err := json.Unmarshal(raw, &body); err != nil || body.Helper.Task != "Inspect changes" || body.Helper.Goober != "coder" || body.Helper.Base != "parent" {
			t.Fatalf("body=%s error=%v", raw, err)
		}
		if strings.Contains(string(raw), "invocationKey") {
			t.Fatal("invocation key must remain a header")
		}
		return childHTTPResponse(http.StatusAccepted, `{"state":"queued","invocationKey":"inspect"}`), nil
	})
	result, out := childRPC(t, server, "start_helper", json.RawMessage(`{"task":"Inspect changes","goober":"coder","invocationKey":"inspect","base":"parent"}`))
	if result["isError"] == true || calls != 1 {
		t.Fatalf("helper: %s calls=%d", out, calls)
	}
	for _, raw := range []string{
		`{"task":"x","goober":"coder","invocationKey":"inspect","grant":"forged"}`,
		`{"task":"x","task":"y","goober":"coder","invocationKey":"inspect"}`,
		`{"task":"x","goober":"coder","invocationKey":"inspect","sourceFile":"private"}`,
		`{"task":"x","invocationKey":"inspect"}`,
		`{"task":"` + strings.Repeat("x", childworkflowwire.MaxHelperTaskBytes+1) + `","goober":"coder","invocationKey":"inspect"}`,
	} {
		result, _ := childRPC(t, server, "start_helper", json.RawMessage(raw))
		if result["isError"] != true || calls != 1 {
			t.Fatalf("invalid helper reached transport: calls=%d result=%v", calls, result)
		}
	}
	server.tools.cfg.ChildWorkflows = nil
	result, _ = childRPC(t, server, "start_helper", json.RawMessage(`{"task":"x","goober":"coder","invocationKey":"inspect"}`))
	if result["isError"] != true || calls != 1 {
		t.Fatal("ungranted helper reached transport")
	}
}
