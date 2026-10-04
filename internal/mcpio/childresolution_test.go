package mcpio

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract/childworkflowwire"
)

func TestChildResolutionToolUsesOwnGrantAndExactResult(t *testing.T) {
	server, _ := newChildServer(t)
	digest := "sha256:" + strings.Repeat("a", 64)
	calls := 0
	server.tools.childTransport = childRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/v1/runs/run-123/child-workflows/resolve" || r.Header.Get("Authorization") != "Bearer "+childMCPGrant || r.Header.Get("Idempotency-Key") != "" {
			t.Fatal("resolution changed trusted scope")
		}
		var body childworkflowwire.ChildWorkflowResolveRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.InvocationKey != "inspect" || body.Action != "replace" || body.ResultRef != digest {
			t.Fatal(body, err)
		}
		return childHTTPResponse(202, `{"invocationKey":"inspect","action":"replace","resultRef":"`+digest+`","applied":false}`), nil
	})
	args := `{"invocationKey":"inspect","action":"replace","resultRef":"` + digest + `"}`
	result, out := childRPC(t, server, "resolve_child_workflow", json.RawMessage(args))
	if result["isError"] == true || !strings.Contains(out, `applied`) || calls != 1 {
		t.Fatal(out, calls)
	}
	for _, invalid := range []string{strings.Replace(args, "replace", "commit", 1), strings.Replace(args, digest, "other", 1), strings.Replace(args, `"action":"replace"`, `"action":"replace","runId":"foreign"`, 1)} {
		result, _ := childRPC(t, server, "resolve_child_workflow", json.RawMessage(invalid))
		if result["isError"] != true || calls != 1 {
			t.Fatal("invalid resolution sent", invalid)
		}
	}
}
