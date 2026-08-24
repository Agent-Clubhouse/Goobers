package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

func TestRunDeclaredStageCommandSuccessCapturesOutput(t *testing.T) {
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","echo hello; echo world >&2"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	var stdout, stderr bytes.Buffer
	result := runDeclaredStage(context.Background(), &stdout, &stderr)
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %q, want success (result %+v)", result.Status, result)
	}
	if result.Outputs["stdout"] != "hello\n" {
		t.Fatalf("captured stdout = %q", result.Outputs["stdout"])
	}
	if result.Outputs["stderr"] != "world\n" {
		t.Fatalf("captured stderr = %q", result.Outputs["stderr"])
	}
	if !bytes.Contains(stdout.Bytes(), []byte("hello")) {
		t.Fatal("stage stdout was not also relayed to the process's own stdout (kubectl logs would see nothing)")
	}
}

func TestRunDeclaredStageCommandFailureReportsExitCode(t *testing.T) {
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","echo boom >&2; exit 3"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "stage_failed" {
		t.Fatalf("error = %+v, want stage_failed", result.Error)
	}
	if result.Outputs["stderr"] != "boom\n" {
		t.Fatalf("captured stderr = %q", result.Outputs["stderr"])
	}
}

func TestRunDeclaredStageScriptPath(t *testing.T) {
	t.Setenv(dispatcher.EnvStageCommand, "")
	t.Setenv(dispatcher.EnvStageScript, "echo from-script")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Status != apiv1.ResultSuccess || result.Outputs["stdout"] != "from-script\n" {
		t.Fatalf("result = %+v, want a successful script run", result)
	}
}

// The pod's own environment (which already carries the stage's declared
// Env, stamped there by podspec.go as native container env vars — not
// re-encoded onto GOOBERS_STAGE_*) must reach the executed command.
func TestRunDeclaredStageInheritsProcessEnvironment(t *testing.T) {
	t.Setenv("GOOBERS_TEST_PROBE_VALUE", "carried-through")
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","printf %s \"$GOOBERS_TEST_PROBE_VALUE\""]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Outputs["stdout"] != "carried-through" {
		t.Fatalf("stdout = %q, want the pod's own env var inherited by the exec'd stage", result.Outputs["stdout"])
	}
}

func TestRunDeclaredStageTimeoutIsAFailure(t *testing.T) {
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","sleep 5"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "50ms")

	start := time.Now()
	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("runDeclaredStage took %s, want the 50ms timeout to cut it short", elapsed)
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "stage_timeout" {
		t.Fatalf("result = %+v, want a stage_timeout failure", result)
	}
}

// A malformed GOOBERS_STAGE_COMMAND payload (a version-skewed dispatcher, a
// corrupted env var) fails the STAGE, not the wrapper — dispatch-exec still
// has a well-formed envelope to surrender.
func TestRunDeclaredStageMalformedCommandJSONIsAFailureEnvelope(t *testing.T) {
	t.Setenv(dispatcher.EnvStageCommand, `not valid json`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "stage_declaration_invalid" {
		t.Fatalf("result = %+v, want stage_declaration_invalid", result)
	}
}

// End to end: identity present, daemon reachable, stage runs, and the
// surrendered envelope reaches the write API's surrender plane — the exact
// wiring #3699 was missing. Exit is 0 once the PUT succeeds, regardless of
// the stage's own outcome.
func TestRunDispatchExecContextSurrendersToTheWriteAPI(t *testing.T) {
	var received struct {
		path  string
		token string
		body  []byte
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.path = r.URL.Path
		received.token = r.Header.Get("Authorization")
		received.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	t.Setenv(dispatcher.EnvRunID, "run-1")
	t.Setenv(dispatcher.EnvStage, "probe-builtin")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","echo egress-ok"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	code := runDispatchExecContext(context.Background(), io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 once the surrender PUT succeeds", code)
	}
	if received.path != "/api/v1/runs/run-1/stages/probe-builtin/attempts/1/surrender" {
		t.Fatalf("surrender path = %q", received.path)
	}
	if received.token != "Bearer pod-token" {
		t.Fatalf("surrender auth = %q", received.token)
	}
	var surrendered dispatcher.SurrenderedResult
	if err := json.Unmarshal(received.body, &surrendered); err != nil {
		t.Fatalf("decode surrendered body: %v", err)
	}
	if surrendered.Result.Status != apiv1.ResultSuccess {
		t.Fatalf("surrendered status = %q, want success", surrendered.Result.Status)
	}
}

// A stage that FAILS still surrenders successfully — the wrapper's exit code
// reports whether it could tell the daemon what happened, not what happened.
func TestRunDispatchExecContextSurrendersFailureEnvelopeWithExitZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	t.Setenv(dispatcher.EnvRunID, "run-1")
	t.Setenv(dispatcher.EnvStage, "probe-builtin")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","exit 1"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	if code := runDispatchExecContext(context.Background(), io.Discard, io.Discard); code != 0 {
		t.Fatalf("exit code = %d, want 0 (the failure is IN the surrendered envelope, not the process exit)", code)
	}
}

// Missing pod identity means there is nothing to surrender to: fail loud and
// fast, before ever running the stage — the dispatcher's own gate already
// treats an absent surrender as a retryable infra fault.
func TestRunDispatchExecContextFailsClosedWithoutIdentity(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T){
		"no daemon API": func(t *testing.T) {
			t.Setenv(dispatcher.EnvRunID, "run-1")
			t.Setenv(dispatcher.EnvStage, "probe-builtin")
			t.Setenv(dispatcher.EnvAttempt, "1")
			t.Setenv(dispatcher.EnvDaemonAPI, "")
			t.Setenv(dispatcher.EnvPodToken, "pod-token")
		},
		"no pod token": func(t *testing.T) {
			t.Setenv(dispatcher.EnvRunID, "run-1")
			t.Setenv(dispatcher.EnvStage, "probe-builtin")
			t.Setenv(dispatcher.EnvAttempt, "1")
			t.Setenv(dispatcher.EnvDaemonAPI, "http://127.0.0.1:1")
			t.Setenv(dispatcher.EnvPodToken, "")
		},
		"bad attempt": func(t *testing.T) {
			t.Setenv(dispatcher.EnvRunID, "run-1")
			t.Setenv(dispatcher.EnvStage, "probe-builtin")
			t.Setenv(dispatcher.EnvAttempt, "not-a-number")
			t.Setenv(dispatcher.EnvDaemonAPI, "http://127.0.0.1:1")
			t.Setenv(dispatcher.EnvPodToken, "pod-token")
		},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t)
			t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","echo should-not-run"]`)
			t.Setenv(dispatcher.EnvStageScript, "")
			var stdout bytes.Buffer
			if code := runDispatchExecContext(context.Background(), &stdout, io.Discard); code == 0 {
				t.Fatal("expected a nonzero exit without pod identity")
			}
			if stdout.Len() > 0 {
				t.Fatal("the stage must not run at all when there is no identity to surrender under")
			}
		})
	}
}
