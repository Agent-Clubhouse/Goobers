package mcpio

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSelectedPRToolsRequireDedicatedGrantAndNeverRetry(t *testing.T) {
	tools := sessionToolset(t)
	tools.cfg.SessionOperations.BacklogReadDisabled = true
	server := NewServer(tools)
	if _, err := server.callSessionOperation("inspect_selected_pr", json.RawMessage(`{}`)); err == nil {
		t.Fatal("ungranted repair")
	}
	tools.cfg.SessionOperations.PRRepairEnabled = true
	calls := 0
	tools.sessionTransport = sessionRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/inspect_selected_pr") {
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	count := 0
	for _, def := range server.toolDefs() {
		if isSessionOperationTool(def.Name) {
			count++
		}
	}
	if count != 4 {
		t.Fatal(count)
	}
	if _, err := server.callSessionOperation("inspect_selected_pr", json.RawMessage(`{}`)); err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if _, err := server.callSessionOperation("inspect_selected_pr", json.RawMessage(`{"repository":"foreign"}`)); err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
	tools.cfg.SessionOperations.PRRepairEnabled = false
	if _, err := server.callSessionOperation("inspect_selected_pr", json.RawMessage(`{}`)); err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
