package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
)

// #3279: a caller that does not share the daemon's filesystem must be able to
// trigger a run at all, and must be told when the daemon refuses.

func TestRemoteDaemonAPIBase(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		env   string
		want  string
		fails bool
	}{
		{name: "unset"},
		{name: "flag", flag: "http://daemon.example:8080", want: "http://daemon.example:8080"},
		{name: "flag beats env", flag: "https://a.example", env: "http://b.example", want: "https://a.example"},
		{name: "env fallback", env: "http://daemon.example:8080/", want: "http://daemon.example:8080"},
		{name: "trailing slash trimmed", flag: "https://daemon.example/goobers/", want: "https://daemon.example/goobers"},
		{name: "query dropped", flag: "http://daemon.example?token=x", want: "http://daemon.example"},
		{name: "scheme required", flag: "daemon.example:8080", fails: true},
		{name: "host required", flag: "http://", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(remoteDaemonAPIEnv, tc.env)
			got, err := remoteDaemonAPIBase(tc.flag)
			if tc.fails {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("remoteDaemonAPIBase: %v", err)
			}
			if got != tc.want {
				t.Fatalf("base = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunRemoteTriggerSubmitsToDaemonAPI(t *testing.T) {
	unsetRunContext(t)
	var (
		gotPath    string
		gotMethod  string
		gotAuth    string
		gotKey     string
		gotRequest httpapi.WorkflowStartRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		if serveRemoteRootFixture(w, r) {
			return
		}
		gotKey = r.Header.Get(httpapi.HeaderIdempotencyKey)
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Errorf("decode trigger request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(httpapi.TriggerResponse{RunID: "run-remote-1"})
	}))
	t.Cleanup(server.Close)

	t.Setenv(remoteDaemonAPIEnv, "")
	t.Setenv("GOOBERS_API_TOKEN", "operator-token")
	code, stdout, stderr := runArgs(t, "run", "example/nightly", "--api", server.URL,
		"--request-id", "delivery-1", "--expected-source-revision", "sha256:workflow", "--force", "--no-wait")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if gotMethod != http.MethodPost || gotPath != apicontract.WorkflowStartPath {
		t.Fatalf("request = %s %s, want POST %s", gotMethod, gotPath, apicontract.WorkflowStartPath)
	}
	if gotAuth != "Bearer operator-token" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	want := httpapi.WorkflowStartRequest{Gaggle: "example", Workflow: "nightly", RequestID: "delivery-1", ExpectedSourceRevision: "sha256:workflow", Force: true}
	if gotKey != want.RequestID {
		t.Fatalf("Idempotency-Key = %q, want %q", gotKey, want.RequestID)
	}
	if gotRequest != want {
		t.Fatalf("trigger request = %+v, want %+v", gotRequest, want)
	}
	if !strings.Contains(stdout, "created run run-remote-1") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestRunRemoteTriggerRejectsAcceptanceWithoutRunIdentity(t *testing.T) {
	unsetRunContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRemoteRootFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(httpapi.TriggerResponse{AcceptanceID: "trigger-durable", State: "accepted"})
	}))
	t.Cleanup(server.Close)
	code, stdout, stderr := runArgs(t, "run", "example/nightly", "--api", server.URL, "--request-id", "delivery",
		"--expected-source-revision", "sha256:workflow", "--no-wait")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "no durable run identity") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestRunRemoteTriggerRejectsSuccessWithoutDurableIdentity(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		name := "new"
		if duplicate {
			name = "duplicate"
		}
		t.Run(name, func(t *testing.T) {
			unsetRunContext(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveRemoteRootFixture(w, r) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(httpapi.TriggerResponse{State: "accepted", Duplicate: duplicate})
			}))
			t.Cleanup(server.Close)

			code, stdout, stderr := runArgs(t, "run", "example/nightly", "--api", server.URL, "--request-id", "delivery",
				"--expected-source-revision", "sha256:workflow", "--no-wait")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "no durable run identity") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !strings.Contains(stderr, `retry with --request-id "delivery"`) {
				t.Fatalf("stderr does not preserve the retry identity: %q", stderr)
			}
		})
	}
}

func TestRunRemoteTriggerHonorsConfiguredAcceptanceTimeout(t *testing.T) {
	unsetRunContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRemoteRootFixture(w, r) {
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-t.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	started := time.Now()
	code, _, stderr := runArgs(t, "run", "example/nightly", "--api", server.URL, "--request-id", "timeout-delivery",
		"--expected-source-revision", "sha256:workflow", "--api-timeout", "250ms", "--no-wait")
	if code != 2 || !strings.Contains(stderr, "start outcome is unknown") || !strings.Contains(stderr, `--request-id "timeout-delivery"`) {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("configured short deadline used the old 30-second timeout")
	}
}

func TestRunRemoteTriggerRejectsNonpositiveAPITimeout(t *testing.T) {
	unsetRunContext(t)
	for _, value := range []string{"0s", "-1s"} {
		code, _, stderr := runArgs(t, "run", "example/nightly", "--api", "http://127.0.0.1:1", "--api-timeout="+value)
		if code != 2 || !strings.Contains(stderr, "--api-timeout must be positive") {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
	}
}

func TestRunRemoteTriggerRequiresExpectedSourceRevision(t *testing.T) {
	unsetRunContext(t)
	code, stdout, stderr := runArgs(t, "run", "example/nightly", "--api", "http://daemon.invalid", "--no-wait")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--expected-source-revision is required") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// The endpoint may come from the environment alone, which is how a CI job or a
// stage pod is configured.
func TestRunRemoteTriggerUsesEnvironmentEndpoint(t *testing.T) {
	unsetRunContext(t)
	var gotRequest httpapi.WorkflowStartRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRemoteRootFixture(w, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
			t.Errorf("decode trigger request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(httpapi.TriggerResponse{RunID: "run-remote-2", Duplicate: true})
	}))
	t.Cleanup(server.Close)

	t.Setenv(remoteDaemonAPIEnv, server.URL)
	t.Setenv("GOOBERS_API_TOKEN", "")
	code, stdout, stderr := runArgs(t, "run", "nightly", "--request-id", "delivery-2",
		"--expected-source-revision", "sha256:workflow", "--no-wait")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if gotRequest.Workflow != "nightly" || gotRequest.Gaggle != "" {
		t.Fatalf("trigger request = %+v", gotRequest)
	}
	if !strings.Contains(stdout, "already dispatched run run-remote-2") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// A daemon refusal is a business error (exit 1), reported with the daemon's own
// error envelope rather than swallowed.
func TestRunRemoteTriggerReportsDaemonRefusal(t *testing.T) {
	unsetRunContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRemoteRootFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(apicontract.ErrorEnvelope{
			Error: apicontract.APIError{Code: "gaggle_required", Message: "name the gaggle"},
		})
	}))
	t.Cleanup(server.Close)

	t.Setenv(remoteDaemonAPIEnv, "")
	code, _, stderr := runArgs(t, "run", "nightly", "--api", server.URL,
		"--expected-source-revision", "sha256:workflow", "--no-wait")
	if code != 1 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "gaggle_required") || !strings.Contains(stderr, "name the gaggle") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunRemoteTriggerRejectsInvalidErrorBodyAndRedirect(t *testing.T) {
	t.Run("invalid error body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "{")
		}))
		t.Cleanup(server.Close)

		_, apiErr, err := submitRemoteWorkflowStart(t.Context(), server.URL, httpapi.WorkflowStartRequest{RequestID: "delivery"})
		if apiErr != nil || err == nil || !strings.Contains(err.Error(), "daemon API returned 502 Bad Gateway with an invalid error body") {
			t.Fatalf("api error=%+v error=%v", apiErr, err)
		}
	})

	t.Run("redirect", func(t *testing.T) {
		followed := false
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			followed = true
		}))
		t.Cleanup(target.Close)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		t.Cleanup(server.Close)

		_, apiErr, err := submitRemoteWorkflowStart(t.Context(), server.URL, httpapi.WorkflowStartRequest{RequestID: "delivery"})
		if apiErr != nil || err == nil || !strings.Contains(err.Error(), "daemon API returned 307 Temporary Redirect with an invalid error body") {
			t.Fatalf("api error=%+v error=%v", apiErr, err)
		}
		if followed {
			t.Fatal("mutation redirect was followed")
		}
	})
}

// An unreachable daemon is a transport error (exit 2), never a silent success —
// the silent miss is exactly what the file drop did.
func TestRunRemoteTriggerReportsTransportFailure(t *testing.T) {
	unsetRunContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()

	t.Setenv(remoteDaemonAPIEnv, "")
	code, stdout, stderr := runArgs(t, "run", "nightly", "--api", endpoint,
		"--expected-source-revision", "sha256:workflow", "--no-wait")
	if code != 2 {
		t.Fatalf("exit code = %d, stdout = %q", code, stdout)
	}
	if !strings.Contains(stderr, "call daemon API") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// Waiting for a terminal phase reads the daemon's own journal, which a remote
// caller does not have; the limit is stated instead of silently ignored.
func TestRunRemoteTriggerWithoutNoWaitReportsSubmissionOnly(t *testing.T) {
	unsetRunContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRemoteRootFixture(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(httpapi.TriggerResponse{RunID: "run-remote-3"})
	}))
	t.Cleanup(server.Close)

	t.Setenv(remoteDaemonAPIEnv, server.URL)
	code, stdout, stderr := runArgs(t, "run", "nightly", "--expected-source-revision", "sha256:workflow")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "created run run-remote-3") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "cannot watch the run's journal") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --pr targets a pull request through the local signal path, which the trigger
// plane does not carry; refuse it explicitly rather than dropping the target.
func TestRunRemoteTriggerRefusesPullRequestTarget(t *testing.T) {
	unsetRunContext(t)
	t.Setenv(remoteDaemonAPIEnv, "http://daemon.invalid")
	code, _, stderr := runArgs(t, "run", "merge-review", "--pr", "7", "--no-wait")
	if code != 2 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "--pr is not supported over the daemon API") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunRemoteTriggerRefusesOverlongRequestID(t *testing.T) {
	unsetRunContext(t)
	t.Setenv(remoteDaemonAPIEnv, "http://daemon.invalid")
	oversized := strings.Repeat("a", httpapi.MaxTriggerRequestIDBytes+1)
	code, _, stderr := runArgs(t, "run", "demo", "--request-id", oversized,
		"--expected-source-revision", "sha256:workflow", "--no-wait")
	if code != 2 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "--request-id must be no longer than") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunRemoteTriggerRejectsInvalidEndpoint(t *testing.T) {
	unsetRunContext(t)
	t.Setenv(remoteDaemonAPIEnv, "")
	code, _, stderr := runArgs(t, "run", "nightly", "--api", "daemon.example:8080")
	if code != 2 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "must use http or https") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// The remote flags must survive runFlagArgs' reordering, like --gaggle and --pr.
func TestRunFlagArgsHoistsRemoteFlags(t *testing.T) {
	got := runFlagArgs([]string{"nightly", "--api", "http://daemon.example", "--request-id=delivery-9", "."})
	want := []string{"--api", "http://daemon.example", "--request-id=delivery-9", "nightly", "."}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("runFlagArgs = %v, want %v", got, want)
	}
}

// Escalation resolution is the same primitive as run creation (#3279): it must
// also work against a daemon this process does not share a filesystem with, so
// no instance root is read when an endpoint is configured.
func TestApproveUsesRemoteDaemonAPI(t *testing.T) {
	unsetRunContext(t)
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRemoteRootFixture(w, r) {
			return
		}
		gotPath = r.URL.Path
		if r.Header.Get(httpapi.HeaderIdempotencyKey) == "" {
			t.Errorf("missing idempotency key")
		}
		_ = json.NewEncoder(w).Encode(httpapi.InterventionResult{Phase: "running", State: "stage-1"})
	}))
	t.Cleanup(server.Close)

	t.Setenv(remoteDaemonAPIEnv, "")
	t.Setenv("GOOBERS_API_TOKEN", "operator-token")
	code, stdout, stderr := runArgs(t, "approve", "--api", server.URL, "--actor", "ops", "run-1", "gate-1", t.TempDir())
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(gotPath, "/runs/run-1/stages/gate-1/") {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(stdout, "approve accepted for run run-1") {
		t.Fatalf("stdout = %q", stdout)
	}
}
