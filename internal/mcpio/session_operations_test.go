package mcpio

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/sessioning"
)

type sessionRoundTrip func(*http.Request) (*http.Response, error)

func (f sessionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func sessionToolset(t *testing.T) *Toolset {
	t.Helper()
	return NewToolset(Config{Workspace: t.TempDir(), RunID: "run-1", SessionOperations: &SessionOperationAccess{Endpoint: "https://daemon.invalid", BearerToken: sessioning.OperationTokenPrefix + strings.Repeat("a", 64)}})
}
func TestSessionMCPReadsUseBoundRunAndClosedArguments(t *testing.T) {
	tools := sessionToolset(t)
	calls := 0
	tools.sessionTransport = sessionRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/v1/runs/run-1/session-operations/get_backlog_item" || r.Header.Get("Authorization") != "Bearer "+tools.cfg.SessionOperations.BearerToken {
			t.Fatal("wrong bound transport")
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "actor") || strings.Contains(string(raw), "runId") {
			t.Fatal("authority in body")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"title":"source data"}`))}, nil
	})
	server := NewServer(tools)
	if _, err := server.callTool(json.RawMessage(`{"name":"get_backlog_item","arguments":{"sourceBindingId":"issues","id":"42"}}`)); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{`{"sourceBindingId":"issues","id":"1","runId":"other"}`, `{"sourceBindingId":"issues","id":"1","id":"2"}`} {
		if _, err := server.callSessionOperation("get_backlog_item", json.RawMessage(args)); err == nil {
			t.Fatal("accepted invalid model arguments")
		}
	}
	if calls != 1 {
		t.Fatal("invalid arguments reached server")
	}
	tools.cfg.SessionOperations = nil
	if _, err := server.callSessionOperation("get_backlog_item", json.RawMessage(`{}`)); err == nil {
		t.Fatal("ungranted tool invoked")
	}
	for _, def := range server.toolDefs() {
		if isSessionOperationTool(def.Name) {
			t.Fatal("ungranted tool listed")
		}
	}
}
func TestSessionMCPBoundsRefusesRedirectsAndRedactsGrant(t *testing.T) {
	for _, scenario := range []string{"redirect", "oversize", "secret"} {
		t.Run(scenario, func(t *testing.T) {
			tools := sessionToolset(t)
			calls := 0
			tools.sessionTransport = sessionRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				body := `{"value":"` + tools.cfg.SessionOperations.BearerToken + `"}`
				status := 200
				header := http.Header{}
				if scenario == "redirect" {
					status = 302
					header.Set("Location", "https://foreign.invalid")
				}
				if scenario == "oversize" {
					body = strings.Repeat("x", sessioning.MaxOperationResultBytes+1)
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			result, err := NewServer(tools).callSessionOperation("get_backlog_item", json.RawMessage(`{"sourceBindingId":"issues","id":"42"}`))
			if scenario == "secret" {
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(result)
				if strings.Contains(string(raw), tools.cfg.SessionOperations.BearerToken) {
					t.Fatal("grant exposed")
				}
			} else if err == nil {
				t.Fatal("unsafe response accepted")
			}
			if calls != 1 {
				t.Fatal("redirect followed")
			}
		})
	}
}

func TestSessionMCPNativeWritePreservesEmptySetAndDoesNotRetry(t *testing.T) {
	tools := sessionToolset(t)
	tools.cfg.SessionOperations.BacklogWriteSources = []string{"items"}
	calls := 0
	tools.sessionTransport = sessionRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/edit_backlog_item") {
			t.Fatal(r.URL)
		}
		raw, _ := io.ReadAll(r.Body)
		request, err := sessioning.DecodeBacklogEdit(raw)
		if err != nil || request.Values == nil || request.RequestID != "same-command" {
			t.Fatal(string(raw), err)
		}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":"uncertain"}`))}, nil
	})
	server := NewServer(tools)
	args := json.RawMessage(`{"sourceBindingId":"items","requestId":"same-command","id":"42","sourceId":"99","expectedRevision":"1","field":"labels","values":[]}`)
	if _, err := server.callSessionOperation("edit_backlog_item", args); err == nil {
		t.Fatal("failed request succeeded")
	}
	if calls != 1 {
		t.Fatal("automatic provider retry", calls)
	}
	tools.cfg.SessionOperations.BacklogWriteSources = nil
	if _, err := server.callSessionOperation("edit_backlog_item", args); err == nil || calls != 1 {
		t.Fatal("ungranted write sent")
	}
}
