package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
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
		if serveRemoteRootFixture(w, r) {
			return
		}
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

func TestRunCancelPreservesRetryIdentityOnUnknownResponse(t *testing.T) {
	for _, supplied := range []string{"", "retry-delivery"} {
		t.Run("key="+supplied, func(t *testing.T) {
			t.Setenv(remoteDaemonAPIEnv, "")
			var gotKey string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveRemoteRootFixture(w, r) {
					return
				}
				gotKey = r.Header.Get(httpapi.HeaderIdempotencyKey)
				w.Header().Set("Content-Type", "application/json")
				// The daemon may have executed the cancellation, but the response
				// was truncated. A retry must retain its original operation key.
				_, _ = w.Write([]byte(`{"code":`))
			}))
			defer server.Close()
			args := []string{"run", "cancel", "--api", server.URL}
			if supplied != "" {
				args = append(args, "--request-id", supplied)
			}
			args = append(args, "run-1")
			code, _, stderr := runArgs(t, args...)
			if code != 2 || gotKey == "" || (supplied != "" && supplied != gotKey) {
				t.Fatalf("code=%d key=%q stderr=%q", code, gotKey, stderr)
			}
			if !strings.Contains(stderr, fmt.Sprintf("--request-id=%q", gotKey)) || !strings.Contains(stderr, "unknown") {
				t.Fatalf("missing reconciliation guidance: %q", stderr)
			}
		})
	}
}

func shortenCancelReconcile(t *testing.T) {
	t.Helper()
	window, interval := cancelReconcileWindow, cancelReconcileInterval
	cancelReconcileWindow, cancelReconcileInterval = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { cancelReconcileWindow, cancelReconcileInterval = window, interval })
}

// TestRunCancelConfirmsLandedCancellationAfterLostResponse (#5118): the first
// response is lost although the daemon cancelled the run. Re-asking under the
// same key (first in flight, then complete) reports the real outcome instead
// of "outcome may be unknown".
func TestRunCancelConfirmsLandedCancellationAfterLostResponse(t *testing.T) {
	if !cancelAnswerMayBeLost(&url.Error{Op: "Post", Err: context.DeadlineExceeded}) {
		t.Fatal("a client timeout must be reconciled")
	}
	if cancelAnswerMayBeLost(&url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: errors.New("refused")}}) {
		t.Fatal("a failed dial never sent the request")
	}
	t.Setenv(remoteDaemonAPIEnv, "")
	shortenCancelReconcile(t)
	var (
		mu    sync.Mutex
		calls int
		keys  = map[string]bool{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRemoteRootFixture(w, r) {
			return
		}
		mu.Lock()
		calls++
		call := calls
		keys[r.Header.Get(httpapi.HeaderIdempotencyKey)] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			// Landed, but the connection drops before any answer.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		case 2:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"cancel_outcome_unknown","message":"cancellation is in progress"}}`))
		default:
			_ = json.NewEncoder(w).Encode(httpapi.CancelRunResult{Code: httpapi.CancelCodeAborted, Phase: "aborted"})
		}
	}))
	t.Cleanup(server.Close)

	code, stdout, stderr := runArgs(t, "run", "cancel", "--api", server.URL, "--request-id", "lost-answer", "run-1")
	if code != 0 || !strings.Contains(stdout, "cancelled run run-1") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stderr, "may be unknown") || !strings.Contains(stderr, "confirming cancellation outcome") {
		t.Fatalf("stderr = %q", stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	// net/http may itself replay the dropped first delivery (it carries an
	// idempotency key), so count at least the three distinct answers.
	if calls < 3 || len(keys) != 1 || !keys["lost-answer"] {
		t.Fatalf("calls=%d keys=%v; want >=3 calls under one key", calls, keys)
	}
}

// TestRunCancelStillInFlightAfterWindowReportsUnknown: a cancel the daemon is
// still executing when the reconcile window closes stays "unknown", with the
// same retry identity.
func TestRunCancelStillInFlightAfterWindowReportsUnknown(t *testing.T) {
	t.Setenv(remoteDaemonAPIEnv, "")
	shortenCancelReconcile(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRemoteRootFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"cancel_outcome_unknown","message":"cancellation is in progress"}}`))
	}))
	t.Cleanup(server.Close)

	code, _, stderr := runArgs(t, "run", "cancel", "--api", server.URL, "--request-id", "slow", "run-1")
	if code != 2 || !strings.Contains(stderr, "may be unknown") || !strings.Contains(stderr, `--request-id="slow"`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

// TestRunAbortSubmitsToDaemonAPI: a remote abort is the remote form of the
// existing delegate-to-the-live-daemon path — the daemon terminalizes the run
// rather than this process editing a journal it cannot even open.
func TestRunAbortSubmitsToDaemonAPI(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRemoteRootFixture(w, r) {
			return
		}
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
		{name: "engine cancellation requested", result: httpapi.CancelRunResult{Code: httpapi.CancelCodeRequested}},
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveRemoteRootFixture(w, r) {
					return
				}
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

// TestDaemonCancelServiceResolvesWorkflowAndReleasesSlot pins the path a
// remote cancel takes that the file drop never does: runRemoteCancel sends no
// `workflow`, so the daemon must recover it from its own registry and hand it
// to the scheduler's release. Without that the run is cancelled but its
// admission slot is never freed, and the workflow's concurrency budget leaks a
// slot per remote cancel.
func TestDaemonCancelServiceResolvesWorkflowAndReleasesSlot(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	deterministic := &liveStalledDeterministic{started: make(chan struct{})}
	runRunner, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return deterministic, nil
		},
		Worktrees:  manager,
		ScratchDir: filepath.Join(layout.WorkcopiesDir(), "scratch"),
		RunsDir:    layout.RunsDir(),
		FinalizeTerminal: func(runID string, _ journal.RunPhase) error {
			return finalizeTerminalRun(layout, log, manager, runID)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runners := newDaemonRunnerRegistry()
	runners.Replace(map[string]*runner.Runner{"example": runRunner})
	machine, err := workflow.Compile(workflow.Definition{
		Name: "implementation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example",
			Start:  "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "block until cancelled",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: workflow.TerminalComplete,
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}

	var tracked sync.WaitGroup
	sched := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow:  "implementation",
		Gaggle:    "example",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   &trackedStarter{r: runRunner, machine: machine, wg: &tracked, l: layout, log: log, runners: runners},
	}}, log)
	runID, err := sched.Trigger(context.Background(), "implementation", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-deterministic.started:
	case <-time.After(5 * time.Second):
		t.Fatal("live run did not enter its attempt")
	}

	var (
		mu             sync.Mutex
		releasedRun    string
		releasedFlow   string
		releasedCalled bool
	)
	service := newDaemonCancelService(runners)
	service.AttachRelease(func(id, workflow string) {
		mu.Lock()
		releasedRun, releasedFlow, releasedCalled = id, workflow, true
		mu.Unlock()
		sched.ReleaseRun(id, workflow)
	})

	result, err := service.Cancel(context.Background(), httpapi.CancelRunRequest{RunID: runID, Actor: "ops"})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if result.Code != httpapi.CancelCodeAborted || result.Phase != string(journal.PhaseAborted) {
		t.Fatalf("result = %+v, want aborted", result)
	}

	sched.Wait()
	tracked.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !releasedCalled {
		t.Fatal("scheduler release was never invoked, so the admission slot leaked")
	}
	if releasedRun != runID || releasedFlow != "implementation" {
		t.Fatalf("release(%q, %q), want (%q, %q)", releasedRun, releasedFlow, runID, "implementation")
	}
	assertWatchdogPhase(t, layout.RunsDir(), runID, journal.PhaseAborted)
}
