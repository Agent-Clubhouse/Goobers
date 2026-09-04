package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
)

// #3807: `run cancel` and `run abort` reached a daemon only through
// <SchedulerDir>/pending-cancels/, so stopping a run required sharing the
// daemon's filesystem. With --api (or $GOOBERS_DAEMON_API) they speak the
// daemon's authenticated API instead; with no endpoint configured the file
// drop is unchanged.

func TestRunCancelSubmitsToDaemonAPI(t *testing.T) {
	var (
		gotPath   string
		gotMethod string
		gotKey    string
		gotAuth   string
		gotBody   httpapi.CancelRunRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotKey = r.Header.Get(httpapi.HeaderIdempotencyKey)
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode cancel request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(httpapi.CancelRunResult{Code: httpapi.CancelCodeAborted, Phase: "aborted"})
	}))
	t.Cleanup(server.Close)

	t.Setenv(remoteDaemonAPIEnv, "")
	t.Setenv("GOOBERS_API_TOKEN", "operator-token")
	code, stdout, stderr := runArgs(t, "run", "cancel", "--api", server.URL, "run-1")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/runs/run-1/cancel" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotKey == "" {
		t.Fatalf("cancel carried no idempotency key")
	}
	if gotAuth != "Bearer operator-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if strings.TrimSpace(gotBody.Actor) == "" {
		t.Fatalf("cancel named no actor: %+v", gotBody)
	}
	if !strings.Contains(stdout, "cancelled run run-1") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestRunAbortSubmitsToDaemonAPI: a remote abort is the remote form of the
// existing delegate-to-the-live-daemon path — the daemon terminalizes the run
// rather than this process editing a journal it cannot even open.
func TestRunAbortSubmitsToDaemonAPI(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(httpapi.CancelRunResult{Code: httpapi.CancelCodeAborted, Phase: "aborted"})
	}))
	t.Cleanup(server.Close)

	t.Setenv("GOOBERS_API_TOKEN", "")
	t.Setenv(remoteDaemonAPIEnv, server.URL)
	code, stdout, stderr := runArgs(t, "run", "abort", "run-2")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if gotPath != "/api/v1/runs/run-2/cancel" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(stdout, "aborted run run-2") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestRunCancelRemoteDispositionsMapToExitCodes keeps the remote path's exit
// codes identical to the file-drop path's: a daemon that refuses is 1, not a
// silent success.
func TestRunCancelRemoteDispositionsMapToExitCodes(t *testing.T) {
	tests := []struct {
		name   string
		result httpapi.CancelRunResult
		want   int
		stderr string
	}{
		{name: "aborted", result: httpapi.CancelRunResult{Code: httpapi.CancelCodeAborted, Phase: "aborted"}},
		{
			name:   "already terminal",
			result: httpapi.CancelRunResult{Code: httpapi.CancelCodeTerminal, Phase: "completed"},
			want:   1,
			stderr: "finished before it could be cancelled",
		},
		{
			name:   "not running",
			result: httpapi.CancelRunResult{Code: httpapi.CancelCodeNotRunning},
			want:   1,
			stderr: "not currently running",
		},
		{
			name:   "daemon error",
			result: httpapi.CancelRunResult{Error: "cancel delegate: malformed request"},
			want:   1,
			stderr: "malformed request",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(tc.result)
			}))
			t.Cleanup(server.Close)

			t.Setenv(remoteDaemonAPIEnv, "")
			code, _, stderr := runArgs(t, "run", "cancel", "--api", server.URL, "run-3")
			if code != tc.want {
				t.Fatalf("exit code = %d, want %d (stderr = %q)", code, tc.want, stderr)
			}
			if tc.stderr != "" && !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("stderr = %q, want %q", stderr, tc.stderr)
			}
		})
	}
}

func TestRunCancelRejectsInvalidEndpoint(t *testing.T) {
	t.Setenv(remoteDaemonAPIEnv, "")
	code, _, stderr := runArgs(t, "run", "cancel", "--api", "daemon.example:8080", "run-1")
	if code != 2 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "must use http or https") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestDaemonCancelServiceAnswersUnownedRun pins the daemon-side half: the
// plane runs the same executeCancelRequest the file-drop sweep runs, so a run
// this daemon is not executing is refused with a code rather than having its
// journal edited behind a would-be owner's back.
func TestDaemonCancelServiceAnswersUnownedRun(t *testing.T) {
	service := newDaemonCancelService(newDaemonRunnerRegistry())
	result, err := service.Cancel(context.Background(), httpapi.CancelRunRequest{RunID: "run-1", Actor: "ops"})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if result.Code != httpapi.CancelCodeNotRunning || result.Error == "" {
		t.Fatalf("result = %+v", result)
	}
}
