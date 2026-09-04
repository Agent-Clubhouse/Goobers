package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
)

// #3807: an escalated run must be resolvable by an operator who does not share
// the daemon's filesystem — the route existed, the verb did not.

func TestEscalationResolveSubmitsToDaemonAPI(t *testing.T) {
	var (
		gotPath    string
		gotMethod  string
		gotKey     string
		gotAuth    string
		gotRequest httpapi.EscalationResolutionRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotKey = r.Header.Get(httpapi.HeaderIdempotencyKey)
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Errorf("decode escalation request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(httpapi.InterventionResult{Phase: "running", State: "implement", JournalSeq: 12})
	}))
	t.Cleanup(server.Close)

	t.Setenv(remoteDaemonAPIEnv, "")
	t.Setenv("GOOBERS_API_TOKEN", "operator-token")
	code, stdout, stderr := runArgs(t, "escalations", "resolve",
		"--resolution", "approve", "--gate", "review", "--actor", "ops",
		"--api", server.URL, "run-1")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/runs/run-1/escalation/resolve" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotKey == "" {
		t.Fatalf("resolution carried no idempotency key")
	}
	if gotAuth != "Bearer operator-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotRequest.Resolution != httpapi.EscalationResolutionApprove || gotRequest.Gate != "review" || gotRequest.Actor != "ops" {
		t.Fatalf("request = %+v", gotRequest)
	}
	if !strings.Contains(stdout, "phase=running") || !strings.Contains(stdout, "state=implement") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestEscalationResolveReadsEndpointFromEnvironment pins the same
// $GOOBERS_DAEMON_API fallback the other remote-capable verbs use.
func TestEscalationResolveReadsEndpointFromEnvironment(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(httpapi.InterventionResult{Phase: "aborted"})
	}))
	t.Cleanup(server.Close)

	t.Setenv(remoteDaemonAPIEnv, server.URL)
	t.Setenv("GOOBERS_API_TOKEN", "")
	code, _, stderr := runArgs(t, "escalations", "resolve",
		"--resolution", "deny", "--rationale", "not shippable", "--actor", "ops", "run-2")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if gotPath != "/api/v1/runs/run-2/escalation/resolve" {
		t.Fatalf("path = %q", gotPath)
	}
}

// TestEscalationResolveReportsDaemonRefusal keeps a refusal distinguishable
// from a usage error: refused is 1, malformed invocation is 2.
func TestEscalationResolveReportsDaemonRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(apicontract.ErrorEnvelope{
			Error: apicontract.APIError{Code: "not_escalated", Message: "run run-3 is not escalated"},
		})
	}))
	t.Cleanup(server.Close)

	t.Setenv(remoteDaemonAPIEnv, "")
	code, _, stderr := runArgs(t, "escalations", "resolve",
		"--resolution", "approve", "--gate", "review", "--actor", "ops", "--api", server.URL, "run-3")
	if code != 1 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "not_escalated") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestEscalationResolveRejectsIncompleteInvocations(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no resolution", args: []string{"run-1"}, want: "--resolution is required"},
		{name: "unknown resolution", args: []string{"--resolution", "shrug", "run-1"}, want: "must be approve, deny, or redirect"},
		{name: "approve without gate", args: []string{"--resolution", "approve", "run-1"}, want: "--gate is required"},
		{
			name: "redirect without decision",
			args: []string{"--resolution", "redirect", "--gate", "review", "--rationale", "r", "run-1"},
			want: "--decision is required",
		},
		{
			name: "deny without rationale",
			args: []string{"--resolution", "deny", "run-1"},
			want: "--rationale is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(remoteDaemonAPIEnv, "http://daemon.invalid")
			code, _, stderr := runArgs(t, append([]string{"escalations", "resolve"}, tc.args...)...)
			if code != 2 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr = %q, want %q", stderr, tc.want)
			}
		})
	}
}
