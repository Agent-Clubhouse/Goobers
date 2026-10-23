package operatorcli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func runForTest(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	var matched Command
	consumed := 0
	for _, candidate := range Commands() {
		parts := strings.Fields(candidate.Name)
		if len(parts) <= consumed || len(parts) > len(args) {
			continue
		}
		ok := true
		for i := range parts {
			if parts[i] != args[i] {
				ok = false
				break
			}
		}
		if ok {
			matched, consumed = candidate, len(parts)
		}
	}
	if consumed == 0 {
		return 2, "", "unknown command"
	}
	code := matched.Run(args[consumed:], &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestOperatorReadsUseRegisteredAPIRoutes(t *testing.T) {
	t.Setenv("GOOBERS_API_TOKEN", "operator-token")
	requests := make(chan *http.Request, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests <- request.Clone(request.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	for _, test := range []struct {
		args []string
		path string
	}{
		{args: []string{"status", "--api", server.URL}, path: apicontract.InstancePath},
		{args: []string{"runs", "list", "--api", server.URL, "--gaggle", "example", "--limit", "3"}, path: apicontract.RunsPath},
		{args: []string{"runs", "show", "--api", server.URL, "run-1"}, path: strings.ReplaceAll(apicontract.RunDetailPath, "{run}", "run-1")},
		{args: []string{"runs", "logs", "--api", server.URL, "run-1"}, path: strings.ReplaceAll(apicontract.RunEventsPath, "{run}", "run-1")},
	} {
		code, stdout, stderr := runForTest(test.args...)
		if code != 0 || stderr != "" || stdout != "{\"ok\":true}\n" {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", test.args, code, stdout, stderr)
		}
		request := <-requests
		if request.Method != http.MethodGet || request.URL.Path != test.path {
			t.Fatalf("%v request = %s %s", test.args, request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer operator-token" {
			t.Fatalf("%v authorization = %q", test.args, request.Header.Get("Authorization"))
		}
	}
}

func TestOperatorMutationsCarryTypedBodyAndIdempotency(t *testing.T) {
	type observed struct {
		method string
		path   string
		key    string
		body   map[string]any
	}
	requests := make(chan observed, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		requests <- observed{method: request.Method, path: request.URL.Path, key: request.Header.Get("Idempotency-Key"), body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	t.Cleanup(server.Close)

	tests := []struct {
		args     []string
		path     string
		key      string
		bodyKey  string
		bodyWant any
	}{
		{
			args: []string{"triggers", "--api", server.URL, "--workflow", "implement", "--gaggle", "example", "--idempotency-key", "trigger-1"},
			path: apicontract.TriggerIngestPath, key: "trigger-1", bodyKey: "workflow", bodyWant: "implement",
		},
		{
			args: []string{"approve", "--api", server.URL, "--idempotency-key", "approve-1", "--actor", "operator", "run-1", "review"},
			path: "/api/v1/runs/run-1/stages/review/approve", key: "approve-1", bodyKey: "actor", bodyWant: "operator",
		},
		{
			args: []string{"override", "--api", server.URL, "--idempotency-key", "override-1", "--decision", "pass", "--rationale", "reviewed", "run-1", "review"},
			path: "/api/v1/runs/run-1/stages/review/override", key: "override-1", bodyKey: "decision", bodyWant: "pass",
		},
		{
			args: []string{"cancel", "--api", server.URL, "--idempotency-key", "cancel-1", "--gaggle", "example", "run-1"},
			path: "/api/v1/runs/run-1/cancel", key: "cancel-1", bodyKey: "gaggle", bodyWant: "example",
		},
	}
	for _, test := range tests {
		code, _, stderr := runForTest(test.args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: code=%d stderr=%q", test.args, code, stderr)
		}
		got := <-requests
		if got.method != http.MethodPost || got.path != test.path || got.key != test.key || got.body[test.bodyKey] != test.bodyWant {
			t.Fatalf("%v request = %+v", test.args, got)
		}
	}
}

func TestOperatorCommandsFailClosed(t *testing.T) {
	for _, test := range []struct {
		args  []string
		issue string
	}{
		{args: []string{"claims", "release"}, issue: "#7845"},
		{args: []string{"config", "apply"}, issue: "#7834"},
		{args: []string{"config", "status"}, issue: "#7834"},
	} {
		code, stdout, stderr := runForTest(test.args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "not available until "+test.issue) {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", test.args, code, stdout, stderr)
		}
	}
}

func TestOperatorErrorsAreTypedAndAPIIsRequired(t *testing.T) {
	if code, _, stderr := runForTest("status"); code != 2 || !strings.Contains(stderr, "--api or GOOBERS_CLOUD_API is required") {
		t.Fatalf("missing API: code=%d stderr=%q", code, stderr)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(apicontract.ErrorEnvelope{Error: apicontract.APIError{Code: "request_conflict", Message: "request already belongs to another operation"}})
	}))
	t.Cleanup(server.Close)
	code, _, stderr := runForTest("status", "--api", server.URL)
	if code != 1 || !strings.Contains(stderr, "request_conflict") || !strings.Contains(stderr, "request already belongs") {
		t.Fatalf("typed API error: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := runForTest("status", "--api", "https://user:secret@example.com"); code != 2 || !strings.Contains(stderr, "without credentials") || strings.Contains(stderr, "secret") {
		t.Fatalf("credential-bearing API URL: code=%d stderr=%q", code, stderr)
	}
}

func TestDeferredOperatorHelpUsesRegistry(t *testing.T) {
	code, stdout, stderr := runForTest("claims", "release", "--help")
	if code != 0 || stderr != "" || stdout != claimsReleaseHelp {
		t.Fatalf("deferred help: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestOperatorRegistryCoversPlannedSurface(t *testing.T) {
	want := []string{
		"status", "runs list", "runs show", "runs logs",
		"claims list", "claims release", "triggers",
		"approve", "override", "deny", "cancel",
		"config apply", "config status", "migrate status", "lease status",
	}
	registered := make(map[string]Command)
	for _, candidate := range Commands() {
		registered[candidate.Name] = candidate
	}
	for _, name := range want {
		candidate, ok := registered[name]
		if !ok || candidate.Run == nil || strings.TrimSpace(candidate.Long) == "" {
			t.Errorf("operator command %q is not fully registered", name)
		}
	}
}
