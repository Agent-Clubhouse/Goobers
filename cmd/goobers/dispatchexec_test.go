package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runnercap"
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

// Decision 003 ruling 3, pod-entrypoint backstop: a ledger-touching
// goobers-CLI command reaching the pod with GOOBERS_INSTANCE_ROOT unset
// (which the dispatcher never stamps — the engine's dispatchRemoteTask is
// supposed to have refused this before a pod existed) is refused HERE too,
// before any credential resolution or checkout is attempted.
func TestRunDeclaredStageRefusesLedgerCommandWithoutInstanceRoot(t *testing.T) {
	t.Setenv(dispatcher.EnvStageCommand, `["goobers","select-source"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")
	t.Setenv("GOOBERS_INSTANCE_ROOT", "")

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "instance_root_required" {
		t.Fatalf("result = %+v, want an instance_root_required failure", result)
	}
	if !strings.Contains(result.Error.Message, "select-source") {
		t.Fatalf("error message = %q, want it to name the refused command", result.Error.Message)
	}
}

// The kind-based half of the same backstop: inputs.kind=external-telemetry
// has no pod-side execution path regardless of GOOBERS_INSTANCE_ROOT (its
// executor is built from the instance's connector configuration, which lives
// under a config directory a pod does not have), and the dispatcher stamps a
// declared input as GOOBERS_INPUT_<KEY> exactly as the local executor does
// (buildStageEnv), so GOOBERS_INPUT_KIND is what a real external-telemetry
// pod would actually carry.
//
// ci-poll used to be this test's subject and deliberately is not any more:
// #3881 gave it an in-pod path (dispatchcipoll.go), and
// TestRunDeclaredStageNoLongerRefusesCIPoll is the ablation that pins the
// removal.
func TestRunDeclaredStageRefusesKindWithoutInstanceRoot(t *testing.T) {
	t.Setenv(dispatcher.EnvStageCommand, `["goobers","external-telemetry"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")
	t.Setenv("GOOBERS_INSTANCE_ROOT", "")
	t.Setenv("GOOBERS_INPUT_KIND", "external-telemetry")

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "instance_root_required" {
		t.Fatalf("result = %+v, want an instance_root_required failure", result)
	}
}

// An ordinary command with no ledger/journal reach — the common case, `make
// ci` and every provider-only CLI stage — must never trip the backstop just
// because GOOBERS_INSTANCE_ROOT happens to be unset (true for every pod
// today).
func TestRunDeclaredStageOrdinaryCommandIgnoresMissingInstanceRoot(t *testing.T) {
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","true"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")
	t.Setenv("GOOBERS_INSTANCE_ROOT", "")

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Error != nil && result.Error.Code == "instance_root_required" {
		t.Fatalf("result = %+v, an unrelated command must not trip the instance-root backstop", result)
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

// #4260: the surrender retry deadline scales with the stage's own declared
// timeout, with a floor so even a short-timeout stage gets a meaningful
// retry window across a routine daemon restart (#3809).
func TestSurrenderRetryDeadline(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"shorter than the floor uses the floor", 2 * time.Minute, surrenderRetryFloor},
		{"zero (unset) uses the floor", 0, surrenderRetryFloor},
		{"exactly the floor stays at the floor", surrenderRetryFloor, surrenderRetryFloor},
		{"longer than the floor scales with the stage", 90 * time.Minute, 90 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := surrenderRetryDeadline(c.timeout); got != c.want {
				t.Fatalf("surrenderRetryDeadline(%s) = %s, want %s", c.timeout, got, c.want)
			}
		})
	}
}

// #4260: a stage pod's surrender PUT must survive the daemon restart that
// happens on every rollout, not discard an already-finished attempt on the
// first refused or dropped connection.
func TestRunDispatchExecContextSurrenderRetriesTransientFailure(t *testing.T) {
	// Two failures against the production backoff (internal/dispatcher's
	// retryBaseDelay/retryMaxDelay) cost at most ~1.5s of jittered wait —
	// bounded and worth spending here to exercise the real default rather
	// than a shrunk one, unlike the dispatcher package's own unit tests
	// (which shrink the backoff to assert on deadline expiry precisely).
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	t.Setenv(dispatcher.EnvRunID, "run-1")
	t.Setenv(dispatcher.EnvStage, "probe-builtin")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	// No stdout/stderr: any captured output would also record a stage
	// artifact through the SAME server via a journal-plane Emit call
	// (recordStageArtifactsTyped), confounding this test's attempt count —
	// unlike TestRunDispatchExecContextSurrendersToTheWriteAPI, which only
	// checks the surrender body and doesn't care how many requests it took.
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","exit 0"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	code := runDispatchExecContext(context.Background(), io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 once the retried surrender PUT succeeds", code)
	}
	if attempts != 3 {
		t.Fatalf("server saw %d attempts, want 3 (two transient failures, then success)", attempts)
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

// The measured defect this fixes: a stage emitted {"verdict":"pass"} into its
// declared result file, the pod surrendered only stdout, and a gate reading the
// `verdict` output key took its FAILURE branch — three times — while the run
// reported completed. Outputs must carry the result file's scalars.
func TestDispatchExecLiftsResultFileIntoOutputs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "r.json"), []byte(`{"verdict":"pass","count":3,"ok":true,"nested":{"x":1}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	outputs := map[string]interface{}{"stdout": "ignored"}
	data, err := os.ReadFile(filepath.Join(dir, "r.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	mergeResultFileOutputs(outputs, data)

	if outputs["verdict"] != "pass" {
		t.Fatalf("verdict = %v, want pass — a gate reading this key is the reason it matters", outputs["verdict"])
	}
	if outputs["count"] != float64(3) || outputs["ok"] != true {
		t.Fatalf("scalars not lifted: %v", outputs)
	}
	// Structured values are NOT lifted — the local executor lifts only
	// scalars, and diverging here would change gate behaviour by substrate.
	if _, present := outputs["nested"]; present {
		t.Fatalf("nested object must not be lifted (local executor lifts scalars only): %v", outputs)
	}
	if outputs["stdout"] != "ignored" {
		t.Fatalf("existing outputs must survive the merge: %v", outputs)
	}
}

// Invalid JSON is ignored rather than failing the stage — matching the local
// executor. A stage that writes a non-JSON result file still reports its own
// exit status.
func TestDispatchExecIgnoresUnparseableResultFile(t *testing.T) {
	outputs := map[string]interface{}{}
	mergeResultFileOutputs(outputs, []byte("not json at all"))
	if len(outputs) != 0 {
		t.Fatalf("unparseable result file must contribute nothing, got %v", outputs)
	}
}

// A stage must never receive the dispatcher's control plane — above all
// GOOBERS_POD_TOKEN, which authorizes surrendering THIS run's results. A stage
// that can read it can report success for work that failed.
//
// MEASURED before this fix, inside a real pod on a runner declaring
// env:default-deny: POD_TOKEN=PRESENT, DAEMON_API=PRESENT, TOTAL_ENV=24.
func TestStageEnvironmentDropsDispatcherControlPlane(t *testing.T) {
	t.Setenv("GOOBERS_POD_TOKEN", "goobers-pod.secret")
	t.Setenv("GOOBERS_DAEMON_API", "https://daemon.invalid:8080")
	t.Setenv("GOOBERS_RUN_ID", "run-1")
	t.Setenv("GOOBERS_STAGE_COMMAND", `["sh","-c","true"]`)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "r.json")
	t.Setenv("GOOBERS_INPUT_PROBEVALUE", "42")

	got := map[string]string{}
	for _, kv := range stageEnvironment() {
		name, value, _ := strings.Cut(kv, "=")
		got[name] = value
	}

	for _, banned := range dispatcher.DispatcherControlEnv {
		if _, present := got[banned]; present {
			t.Fatalf("stage environment leaks dispatcher control variable %q", banned)
		}
	}
	// Declared inputs ARE the stage's to read.
	if got["GOOBERS_INPUT_RESULTFILE"] != "r.json" || got["GOOBERS_INPUT_PROBEVALUE"] != "42" {
		t.Fatalf("declared inputs must survive: %v", got)
	}
}

// A pod is disposed after its attempt, so the journal plane is the ONLY way a
// stage's output survives. Recording must be best-effort: the stage has run and
// its ResultEnvelope is authoritative, so a journal failure must not turn a
// completed stage into a failed one — but it must be visible on stderr, not
// swallowed.
func TestRecordStageArtifactsIsBestEffortAndVisible(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, "run-7")
	t.Setenv(dispatcher.EnvStage, "build")
	t.Setenv(dispatcher.EnvAttempt, "2")
	t.Setenv(dispatcher.EnvGaggle, "g")

	var errOut strings.Builder
	recordStageArtifacts(context.Background(), &errOut, map[string][]byte{
		"stdout.log": []byte("hello"),
		"stderr.log": nil, // empty streams must not be recorded at all
	})
	if !strings.Contains(gotBody, "build/stdout.log") {
		t.Fatalf("emit body must carry the stdout artifact, got %q", gotBody)
	}
	if strings.Contains(gotBody, "stderr.log") {
		t.Fatalf("an EMPTY stream must not be recorded, got %q", gotBody)
	}
	if errOut.Len() != 0 {
		t.Fatalf("a successful record must be silent, got %q", errOut.String())
	}
}

func TestRecordStageArtifactsSurfacesFailureWithoutPanicking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"boom","message":"journal down"}}`))
	}))
	defer server.Close()
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, "run-7")
	t.Setenv(dispatcher.EnvStage, "build")
	t.Setenv(dispatcher.EnvAttempt, "1")

	var errOut strings.Builder
	recordStageArtifacts(context.Background(), &errOut, map[string][]byte{"stdout.log": []byte("x")})
	if !strings.Contains(errOut.String(), "record stage artifacts") {
		t.Fatalf("a journal failure must be VISIBLE on stderr, got %q", errOut.String())
	}
}

// No daemon API (a loopback/no-plane posture) must be a clean no-op rather
// than an error path a stage has to care about.
func TestRecordStageArtifactsNoopsWithoutADaemonAPI(t *testing.T) {
	t.Setenv(dispatcher.EnvDaemonAPI, "")
	t.Setenv(dispatcher.EnvRunID, "run-7")
	t.Setenv(dispatcher.EnvStage, "build")
	var errOut strings.Builder
	recordStageArtifacts(context.Background(), &errOut, map[string][]byte{"stdout.log": []byte("x")})
	if errOut.Len() != 0 {
		t.Fatalf("no daemon API must be a silent no-op, got %q", errOut.String())
	}
}

// The CLI-stage exemption widens what a stage can READ, never what it can DO.
// A goobers-CLI stage keeps its run identity — it cannot name its own run
// branch without it — but the privileged half, above all the pod token, is
// stripped on exactly the same terms as for every other stage. If this test
// ever fails, a CLI stage can author its own run's outcome.
func TestStageEnvironmentCLIStageKeepsIdentityNotAuthority(t *testing.T) {
	t.Setenv("GOOBERS_STAGE_IS_CLI", "true")
	t.Setenv("GOOBERS_POD_TOKEN", "goobers-pod.secret")
	t.Setenv("GOOBERS_DAEMON_API", "https://daemon.invalid:8080")
	t.Setenv("GOOBERS_BLOB_ENDPOINT", "https://daemon.invalid:8080/blobs")
	t.Setenv("GOOBERS_STAGE_COMMAND", `["goobers","backlog-query"]`)
	t.Setenv("GOOBERS_RUN_ID", "run-1")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv("GOOBERS_REPO_OWNER", "Agent-Clubhouse")

	got := map[string]string{}
	for _, kv := range stageEnvironment() {
		name, value, _ := strings.Cut(kv, "=")
		got[name] = value
	}

	for _, banned := range dispatcher.DispatcherPrivilegedEnv {
		if _, present := got[banned]; present {
			t.Fatalf("CLI stage leaks privileged control variable %q", banned)
		}
	}
	for _, kept := range dispatcher.DispatcherRunIdentityEnv {
		if _, present := os.LookupEnv(kept); !present {
			continue // only the ones this test actually set
		}
		if _, present := got[kept]; !present {
			t.Fatalf("CLI stage must keep run identity %q: providers.BranchName composes the run branch from it", kept)
		}
	}
	if got["GOOBERS_REPO_OWNER"] != "Agent-Clubhouse" {
		t.Fatalf("CLI stage must see its routed repo: %v", got["GOOBERS_REPO_OWNER"])
	}
}

// The three categories must remain a PARTITION of the control plane. A name
// that falls out of all of them is a variable that silently stops being
// stripped for every stage — the failure this split could plausibly
// introduce — and a name in two of them is a category boundary that has
// stopped meaning anything.
func TestDispatcherControlEnvIsExactlyItsThreeCategories(t *testing.T) {
	union := map[string]bool{}
	for _, category := range []struct {
		name  string
		names []string
	}{
		{"privileged", dispatcher.DispatcherPrivilegedEnv},
		{"run identity", dispatcher.DispatcherRunIdentityEnv},
		{"machine plane", dispatcher.DispatcherPlaneEnv},
	} {
		for _, n := range category.names {
			if union[n] {
				t.Fatalf("%q is in MORE THAN ONE category (seen again in %s); the split must be a partition", n, category.name)
			}
			union[n] = true
		}
	}
	if len(union) != len(dispatcher.DispatcherControlEnv) {
		t.Fatalf("categories cover %d names, control plane has %d", len(union), len(dispatcher.DispatcherControlEnv))
	}
	for _, n := range dispatcher.DispatcherControlEnv {
		if !union[n] {
			t.Fatalf("%q is in no category: it would stop being stripped", n)
		}
	}
	// The one that matters most, asserted by name rather than by construction.
	privileged := map[string]bool{}
	for _, n := range dispatcher.DispatcherPrivilegedEnv {
		privileged[n] = true
	}
	if !privileged[dispatcher.EnvPodToken] {
		t.Fatal("EnvPodToken must be privileged: it authorizes surrendering this run's results")
	}
	// And the one #3897 turns on: the plane BEARERS are not privileged —
	// a goobers-CLI stage is exactly the party that must read them — but they
	// must also not have been filed as run identity, whose documented
	// justification is that knowing it grants nothing.
	for _, n := range dispatcher.DispatcherPlaneEnv {
		if privileged[n] {
			t.Fatalf("%q is privileged, so a goobers-CLI stage would be stripped of it and silently take the local-file branch", n)
		}
	}
	identity := map[string]bool{}
	for _, n := range dispatcher.DispatcherRunIdentityEnv {
		identity[n] = true
	}
	for _, bearer := range []string{
		dispatcher.ClaimsTokenEnv, dispatcher.StateTokenEnv,
		dispatcher.JournalTokenEnv, dispatcher.TelemetryTokenEnv,
	} {
		if identity[bearer] {
			t.Fatalf("%q is a BEARER filed as run identity; that category's rule is that knowing it grants nothing", bearer)
		}
	}
}

// The pointers a stage pod surrenders are DERIVED from the bytes, never read
// back from the emit response. That is sound only if the derivation matches
// what the daemon's writer produces for the same bytes — otherwise the envelope
// would name a blob that does not exist, which is worse than naming none.
func TestRecordStageArtifactsDerivesTheDaemonsRef(t *testing.T) {
	var got livejournal.EmitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"applied":1}`))
	}))
	defer srv.Close()

	t.Setenv(dispatcher.EnvDaemonAPI, srv.URL)
	t.Setenv(dispatcher.EnvRunID, "run-art")
	t.Setenv(dispatcher.EnvStage, "build")
	t.Setenv(dispatcher.EnvAttempt, "1")

	data := []byte("hello artifact\n")
	var errOut strings.Builder
	pointers := recordStageArtifacts(context.Background(), &errOut, map[string][]byte{"stdout.log": data})

	if len(pointers) != 1 {
		t.Fatalf("expected one pointer, got %d", len(pointers))
	}
	// The independent oracle: what the journal itself computes for these bytes.
	want, err := journal.ArtifactRef(data)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if pointers[0].Digest != want.Digest {
		t.Fatalf("digest = %q, want the journal's %q", pointers[0].Digest, want.Digest)
	}
	if pointers[0].Path != want.Path {
		t.Fatalf("path = %q, want %q", pointers[0].Path, want.Path)
	}
	if pointers[0].Size != want.Size {
		t.Fatalf("size = %d, want %d", pointers[0].Size, want.Size)
	}
	// And the same bytes must actually have been emitted, so the derived
	// pointer and the journal record describe one blob rather than two.
	if len(got.Ops) != 1 || got.Ops[0].Artifact == nil {
		t.Fatalf("expected one artifact op, got %+v", got.Ops)
	}
	if string(got.Ops[0].Artifact.Data) != string(data) {
		t.Fatal("emitted bytes differ from the bytes the pointer was derived from")
	}
}

// An empty stream is not an artifact: emitting a zero-byte blob would put a
// pointer on the envelope for content no one wrote.
func TestRecordStageArtifactsSkipsEmptyStreams(t *testing.T) {
	t.Setenv(dispatcher.EnvDaemonAPI, "")
	var errOut strings.Builder
	if got := recordStageArtifacts(context.Background(), &errOut, map[string][]byte{"stdout.log": nil}); len(got) != 0 {
		t.Fatalf("expected no pointers, got %+v", got)
	}
}

// TestRecordStageArtifactsStampsOpTime is dispatchexec.go's half of #3774,
// tested the same way as podArtifactRecorder.Append's: through the real
// JSON-over-HTTP wire, applied by a REAL *livejournal.Writer onto a REAL
// on-disk journal, then read back — not an in-memory Op.Time assertion,
// which would not cross the boundary the original defect lives on
// (L-152/L-153).
func TestRecordStageArtifactsStampsOpTime(t *testing.T) {
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")

	// recordStageArtifacts, like podArtifactRecorder.Append, carries no Open
	// header — the run must already exist on disk.
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-art-time", Workflow: "impl-real-probe", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) {
		if gaggle != "goobers" {
			return "", false
		}
		return runsDir, true
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req livejournal.EmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp, err := writer.Emit(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "emit_failed", "message": err.Error()}})
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)

	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, "run-art-time")
	t.Setenv(dispatcher.EnvGaggle, "goobers")
	t.Setenv(dispatcher.EnvStage, "build")
	t.Setenv(dispatcher.EnvAttempt, "1")

	before := time.Now().UTC()
	var errOut strings.Builder
	recordStageArtifacts(context.Background(), &errOut, map[string][]byte{"stdout.log": []byte("hello")})
	after := time.Now().UTC()
	if errOut.Len() != 0 {
		t.Fatalf("recordStageArtifacts reported an error: %q", errOut.String())
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-art-time"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := rd.EventRecords()
	if err != nil {
		t.Fatal(err)
	}
	var artifact *journal.EventRecord
	for i := range records {
		if records[i].Event.Name == "build/stdout.log" {
			artifact = &records[i]
		}
	}
	if artifact == nil {
		t.Fatalf("build/stdout.log artifact op never landed in the journal, records = %+v", records)
	}
	if artifact.Event.Time.IsZero() {
		t.Fatal("artifact op event.Time is zero (#3774): recordStageArtifacts must stamp a real wall-clock Time on the Op before it leaves the pod, or the daemon's replayClock zeroes the event it applies")
	}
	if artifact.Event.Time.Before(before) || artifact.Event.Time.After(after) {
		t.Fatalf("artifact op event.Time = %s, want between %s and %s", artifact.Event.Time, before, after)
	}
}

// runStageWithResultFile runs one stage in a temp workspace whose command
// writes the given result-file JSON, then returns the surrendered envelope.
func runStageWithResultFile(t *testing.T, resultJSON string, exitCode int) apiv1.ResultEnvelope {
	t.Helper()
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	script := fmt.Sprintf("printf '%%s' '%s' > out.json; exit %d", resultJSON, exitCode)
	t.Setenv(dispatcher.EnvStageCommand, fmt.Sprintf(`["sh","-c",%q]`, script))
	t.Setenv(dispatcher.InputEnvVar("resultFile"), "out.json")
	t.Setenv(dispatcher.EnvDaemonAPI, "")

	var out, errOut bytes.Buffer
	return runDeclaredStage(context.Background(), &out, &errOut)
}

// noWork is a DISTINCT terminal status. Flattening it to success makes a
// workflow take the did-work path on a pod and the no-work path on a self
// runner, from the same stage and the same outputs.
func TestDispatchExecHonoursNoWork(t *testing.T) {
	got := runStageWithResultFile(t, `{"noWork":true}`, 0)
	if got.Status != apiv1.ResultNoWork {
		t.Fatalf("status = %q, want %q", got.Status, apiv1.ResultNoWork)
	}
	if got.Summary == "" {
		t.Fatal("a no-work stage must carry a summary, as it does locally")
	}
}

// A typed errorCode beats the generic stage_failed — it is the classification a
// retry policy reads, and it was opaque on a pod while specific on self.
func TestDispatchExecHonoursTypedErrorCode(t *testing.T) {
	got := runStageWithResultFile(t, `{"errorCode":"github_rate_limited","errorMessage":"resets at 10:00","errorRetryable":true}`, 1)
	if got.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure", got.Status)
	}
	if got.Error == nil || got.Error.Code != "github_rate_limited" {
		t.Fatalf("error = %+v, want the typed code", got.Error)
	}
	if !got.Error.Retryable {
		t.Fatal("retryable must survive: it is what a retry policy reads")
	}
	// The keys are CONSUMED, not left for downstream stages to trip over.
	for _, k := range []string{"errorCode", "errorMessage", "errorRetryable"} {
		if _, present := got.Outputs[k]; present {
			t.Fatalf("%q must be consumed from Outputs, as the local executor does", k)
		}
	}
}

// exitCode is a measurement of the run and belongs on every envelope.
func TestDispatchExecRecordsExitCodeMetric(t *testing.T) {
	for name, tc := range map[string]struct {
		exit int
		want float64
	}{"success": {0, 0}, "failure": {3, 3}} {
		t.Run(name, func(t *testing.T) {
			got := runStageWithResultFile(t, `{"ok":true}`, tc.exit)
			if got.Metrics == nil {
				t.Fatal("no metrics recorded; a self runner always records exitCode")
			}
			if got.Metrics["exitCode"] != tc.want {
				t.Fatalf("exitCode = %v, want %v", got.Metrics["exitCode"], tc.want)
			}
			if got.Summary == "" {
				t.Fatal("every terminal envelope carries a summary locally")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// env:default-deny (#3725)
// ---------------------------------------------------------------------------

// envDefaultDenyProbe runs one stage through runDeclaredStage with a live
// credential plane and reports what the SUBPROCESS actually saw.
//
// It reads the probe out of the command's own stdout rather than out of
// stageEnvironment(), because the seam #3725 is about is the cmd.Env
// composition — stageEnvironment() plus credEnv plus extraEnv — and a helper-
// level assertion cannot see the append order that makes credentials survive
// the filter (project lesson L-153: the evidence has to come from the far side
// of the process boundary).
//
// Values are reported as PRESENT/ABSENT, never echoed: the same MEASURED
// template the control-plane strip already uses ("POD_TOKEN=PRESENT in a
// 24-variable inherited environment"), and it keeps a credential value out of
// the test's own output.
// The pod's container environment is not hand-written here. It is RENDERED by
// dispatcher.RenderPod from an Attempt and a RunnerSpec and then materialised
// into the test process, because the allowlist is the thing under test: a
// hand-copied GOOBERS_STAGE_ENV_ALLOW asserts what the test author believed
// stageEnvAllowlist() emits, not what it emits. The first cut of this helper
// hand-wrote that list, and that is exactly why its A/B ablation could not see
// the run-identity vars going missing.
//
// cliStage selects the branch of the in-pod control-plane strip: true is the
// goobers-CLI stage that keeps its run identity, false the plain shell stage —
// the common case, which nothing exercised before.
func envDefaultDenyProbe(t *testing.T, defaultDeny, cliStage bool) map[string]string {
	t.Helper()

	const capabilityName = "provider:contents:read"
	credVar := capability.CredentialEnvVar(capabilityName)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apicontract.CredentialResolvePath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"credentials": []map[string]string{{"capability": capabilityName, "value": "minted-at-stage-start"}},
		})
	}))
	t.Cleanup(server.Close)

	probes := []string{
		credVar, "IMAGE_AMBIENT_VAR", "DECLARED_STAGE_VAR", dispatcher.InputEnvVar("probe"),
		"GOOBERS_REPO_NAME", "PATH", dispatcher.EnvPodToken,
		// The run's operational identity. A goobers-CLI stage keeps it by
		// design and cannot name its own run branch without it; every other
		// stage is stripped of it. Absent from the first cut of this list,
		// which is how the allowlist gap reached review (#3725).
		dispatcher.EnvRunID, dispatcher.EnvGaggle, dispatcher.EnvWorkflow,
		dispatcher.EnvStage, dispatcher.EnvAttempt,
	}
	var script strings.Builder
	for _, name := range probes {
		// ${X:+PRESENT} reports presence without ever printing the value, so a
		// credential cannot reach this test's output or a CI log.
		fmt.Fprintf(&script, "printf '%s=%%s\\n' \"${%s:+PRESENT}\"; ", name, name)
	}

	attempt := dispatcher.Attempt{
		RunID:    "run-3725",
		Gaggle:   "e2e",
		Workflow: "implementation",
		Stage:    "cli-on-pod",
		Number:   1,
		Timeout:  30 * time.Second,
		PodToken: "goobers-pod.tok",
		Command:  []string{"sh", "-c", script.String()},
		// What the DISPATCHER stamps for this stage: its declared env, its
		// inputs, and its routed repository.
		Env:          map[string]string{"DECLARED_STAGE_VAR": "from-the-workflow"},
		Inputs:       map[string]string{"probe": "declared-input"},
		RunContext:   map[string]string{"GOOBERS_REPO_NAME": "Goobers"},
		Capabilities: []string{capabilityName},
		CLIStage:     cliStage,
	}
	runner := dispatcher.RunnerSpec{
		Name:     "linux-shell-envdeny",
		OS:       "linux",
		HostKind: instance.RunnerHostImage,
		Host:     "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
	}
	if defaultDeny {
		runner.Restrictions = []string{string(runnercap.RestrictionEnvDefaultDeny)}
	}
	pod, err := dispatcher.RenderPod(dispatcher.Config{
		Namespace:    "gaggle-e2e",
		WriteAPIBase: server.URL,
	}, attempt, runner)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}

	// The container's environment, as the kubelet would set it. Cleared first
	// so a name the dispatcher did NOT stamp for this shape (EnvStageIsCLI on a
	// non-CLI stage, the deny signal on an unrestricted class) cannot arrive
	// from the test process's own environment and quietly select the other
	// branch.
	for _, name := range append([]string{}, dispatcher.DispatcherControlEnv...) {
		t.Setenv(name, "")
	}
	for _, e := range pod.Spec.Containers[0].Env {
		t.Setenv(e.Name, e.Value)
	}
	// What the IMAGE exports. Nothing declared it, nothing allowlists it, and
	// under env:default-deny it is exactly what must not reach the stage — so
	// it is set here, never on the pod spec.
	t.Setenv("IMAGE_AMBIENT_VAR", "baked-into-the-runner-image")

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("probe stage did not run: %+v", result)
	}
	stdout, _ := result.Outputs["stdout"].(string)
	seen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		name, state, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if state == "" {
			state = "ABSENT"
		}
		seen[name] = state
	}
	return seen
}

// #3725: implementing env:default-deny must NOT strip the stage's resolved
// credentials.
//
// The credential plane resolves at stage start and the resulting
// GOOBERS_CRED_<CAP> is appended to cmd.Env AFTER the inherited environment is
// filtered — so it survives by construction, not by being named in an
// allowlist. Routing it through procenv's allowlist instead (the natural
// "build one map, filter once" refactor) drops it, and the failure surfaces at
// GitHub as a 401/404 on one runner class and not another.
func TestEnvDefaultDenyKeepsResolvedCredentials(t *testing.T) {
	seen := envDefaultDenyProbe(t, true, true)
	credVar := capability.CredentialEnvVar("provider:contents:read")
	if seen[credVar] != "PRESENT" {
		t.Fatalf("%s = %q, want PRESENT — env:default-deny stripped the credential the stage just resolved; "+
			"this fails at the PROVIDER (401/404 from GitHub), not here (#3725). Full probe: %v", credVar, seen[credVar], seen)
	}
}

// The other half of the same commit: env:default-deny must actually DENY. An
// ambient variable the image exported, that nothing declared and nothing
// allowlists, must not reach a stage on a class that promises isolation.
func TestEnvDefaultDenyDropsAmbientImageVariables(t *testing.T) {
	seen := envDefaultDenyProbe(t, true, true)
	if seen["IMAGE_AMBIENT_VAR"] != "ABSENT" {
		t.Fatalf("IMAGE_AMBIENT_VAR = %q, want ABSENT — a runner class declaring env:default-deny handed the stage "+
			"the image's ambient environment, which is the restriction being unenforced (#3725). Full probe: %v", seen["IMAGE_AMBIENT_VAR"], seen)
	}
	// The restriction denies the AMBIENT environment, not the stage's own
	// declaration. Its declared env, its inputs, and its routed repository are
	// dispatcher-stamped and must survive, or the fix trades #3725's failure
	// mode for the identical one a seam over.
	for _, name := range []string{"DECLARED_STAGE_VAR", dispatcher.InputEnvVar("probe"), "GOOBERS_REPO_NAME"} {
		if seen[name] != "PRESENT" {
			t.Fatalf("%s = %q, want PRESENT — the dispatcher stamped it for this stage. Full probe: %v", name, seen[name], seen)
		}
	}
	// procenv's own base still applies: without PATH the stage cannot find its
	// toolchain, which is the "browsers were present and INVISIBLE" failure.
	if seen["PATH"] != "PRESENT" {
		t.Fatalf("PATH = %q, want PRESENT — procenv's allowlist is the floor, not an empty environment", seen["PATH"])
	}
	// The control-plane strip still runs after the allowlist rebuild.
	if seen[dispatcher.EnvPodToken] != "ABSENT" {
		t.Fatalf("%s = %q, want ABSENT — the pod token authorizes surrendering this run's results", dispatcher.EnvPodToken, seen[dispatcher.EnvPodToken])
	}
	// A goobers-CLI stage keeps its run identity — it cannot name its own run
	// branch without it. The in-pod rebuild runs BEFORE the CLI/non-CLI split,
	// so a name the dispatcher's allowlist does not carry is gone before the
	// split can keep it: providerRunContext() then fails closed with
	// "GOOBERS_RUN_ID is not set" and providers.BranchName composes nothing,
	// on a declaring class only.
	for _, name := range []string{dispatcher.EnvRunID, dispatcher.EnvGaggle, dispatcher.EnvWorkflow, dispatcher.EnvStage, dispatcher.EnvAttempt} {
		if seen[name] != "PRESENT" {
			t.Fatalf("%s = %q, want PRESENT — env:default-deny dropped run identity a goobers-CLI stage keeps by "+
				"design; open-pr/push-branch/backlog-query on this class fail or name a wrong branch (#3725). "+
				"Full probe: %v", name, seen[name], seen)
		}
	}
}

// The common case, and the branch nothing exercised before review: a PLAIN
// SHELL stage under env:default-deny. It keeps its declared env, its inputs and
// procenv's floor, and it must still lose the whole control plane — run
// identity and run context included, which the allowlist now re-admits by name
// and the strip removes again afterwards.
//
// This is the property that makes allowlisting run identity safe: widening the
// allowlist must not widen what a non-CLI stage can read. A stage running the
// project's own `make ci` seeing the live run's GOOBERS_* is exactly the #322
// perturbation the CLI/non-CLI split exists to prevent.
func TestEnvDefaultDenyNonCLIStageStillLosesTheWholeControlPlane(t *testing.T) {
	seen := envDefaultDenyProbe(t, true, false)
	for _, name := range []string{
		dispatcher.EnvRunID, dispatcher.EnvGaggle, dispatcher.EnvWorkflow, dispatcher.EnvStage,
		dispatcher.EnvAttempt, "GOOBERS_REPO_NAME", dispatcher.EnvPodToken,
	} {
		if seen[name] != "ABSENT" {
			t.Fatalf("%s = %q, want ABSENT — a non-CLI stage is stripped of the whole control plane, and the "+
				"allowlist must not be a way back in (#322/#3725). Full probe: %v", name, seen[name], seen)
		}
	}
	// What it keeps: its own declaration, its inputs, procenv's floor, and the
	// credential it resolved.
	for _, name := range []string{"DECLARED_STAGE_VAR", dispatcher.InputEnvVar("probe"), "PATH", capability.CredentialEnvVar("provider:contents:read")} {
		if seen[name] != "PRESENT" {
			t.Fatalf("%s = %q, want PRESENT — the restriction denies the AMBIENT environment, not the stage's own. "+
				"Full probe: %v", name, seen[name], seen)
		}
	}
	// And the restriction still holds for it.
	if seen["IMAGE_AMBIENT_VAR"] != "ABSENT" {
		t.Fatalf("IMAGE_AMBIENT_VAR = %q, want ABSENT. Full probe: %v", seen["IMAGE_AMBIENT_VAR"], seen)
	}
}

// Parity, and the ablation's other side: a stage on a class WITHOUT
// env:default-deny still inherits the pod's whole environment. The fix is
// additive — nothing changes for the classes every existing stage runs on.
func TestWithoutEnvDefaultDenyTheAmbientEnvironmentIsUnchanged(t *testing.T) {
	seen := envDefaultDenyProbe(t, false, true)
	for _, name := range []string{"IMAGE_AMBIENT_VAR", "DECLARED_STAGE_VAR", dispatcher.InputEnvVar("probe"), "GOOBERS_REPO_NAME", "PATH"} {
		if seen[name] != "PRESENT" {
			t.Fatalf("%s = %q, want PRESENT — a class not declaring env:default-deny must be unaffected. Full probe: %v", name, seen[name], seen)
		}
	}
	if seen[capability.CredentialEnvVar("provider:contents:read")] != "PRESENT" {
		t.Fatal("the credential must reach an unrestricted stage too")
	}
}

// A stage cannot turn its own isolation off. The signal is dispatcher-stamped
// and privileged, so it is stripped from the stage's environment before the
// command runs — a stage that could READ it learns its posture, and one that
// could SET it would disable the restriction it was placed under.
func TestEnvDefaultDenySignalIsNeverVisibleToTheStage(t *testing.T) {
	for _, name := range []string{dispatcher.EnvStageEnvDefaultDeny, dispatcher.EnvStageEnvAllow} {
		if !slices.Contains(dispatcher.DispatcherPrivilegedEnv, name) {
			t.Fatalf("%s must be in DispatcherPrivilegedEnv: a stage that can read or rewrite it authorizes itself", name)
		}
	}

	t.Setenv(dispatcher.EnvStageEnvDefaultDeny, "true")
	t.Setenv(dispatcher.EnvStageEnvAllow, `["DECLARED_STAGE_VAR"]`)
	t.Setenv("DECLARED_STAGE_VAR", "from-the-workflow")
	// Even a goobers-CLI stage, which keeps the run-identity half of the
	// control plane, must not see the privileged half.
	t.Setenv(dispatcher.EnvStageIsCLI, "true")

	for _, kv := range stageEnvironment() {
		name, _, _ := strings.Cut(kv, "=")
		if name == dispatcher.EnvStageEnvDefaultDeny || name == dispatcher.EnvStageEnvAllow {
			t.Fatalf("stage environment leaks the env:default-deny signal %q", name)
		}
	}
}

// The allowlist re-admits variables BY NAME, so a stage whose declared `env:`
// named a control variable would re-admit it into its own environment — unless
// the control-plane strip runs after the rebuild, not before. That ordering is
// the property under test.
func TestEnvDefaultDenyAllowlistCannotReadmitTheControlPlane(t *testing.T) {
	t.Setenv(dispatcher.EnvStageEnvDefaultDeny, "true")
	t.Setenv(dispatcher.EnvStageEnvAllow, `["`+dispatcher.EnvPodToken+`","`+dispatcher.EnvDaemonAPI+`"]`)
	t.Setenv(dispatcher.EnvPodToken, "goobers-pod.tok")
	t.Setenv(dispatcher.EnvDaemonAPI, "https://daemon.invalid:8080")
	t.Setenv(dispatcher.EnvStageIsCLI, "true")

	for _, kv := range stageEnvironment() {
		name, _, _ := strings.Cut(kv, "=")
		if name == dispatcher.EnvPodToken || name == dispatcher.EnvDaemonAPI {
			t.Fatalf("the allowlist re-admitted control variable %q; the control-plane strip must run AFTER the rebuild", name)
		}
	}
}

// An unparseable or missing allowlist must fail CLOSED — procenv's built-in
// base alone, never a fallback to os.Environ().
func TestEnvDefaultDenyFailsClosedOnAnUnparseableAllowlist(t *testing.T) {
	t.Setenv(dispatcher.EnvStageEnvDefaultDeny, "true")
	t.Setenv(dispatcher.EnvStageEnvAllow, "not json at all")
	t.Setenv("IMAGE_AMBIENT_VAR", "baked-into-the-runner-image")

	for _, kv := range stageEnvironment() {
		if name, _, _ := strings.Cut(kv, "="); name == "IMAGE_AMBIENT_VAR" {
			t.Fatal("a malformed allowlist must fall back to procenv's base, not to os.Environ()")
		}
	}
}

// A genuine syncBase base-merge conflict on the pod substrate must classify
// exactly as it does on the self arms (#813, internal/engine/activities.go's
// RunDeterministic and internal/runner/run.go): base_sync_conflict and
// Retryable, so failure-class routes the run into remediation rather than
// burning an implementation repass re-deriving the identical rejected diff
// purely because the stage happened to be pod- rather than worker-placed.
func TestDispatchExecClassifiesSyncBaseConflictAsRetryableInfra(t *testing.T) {
	const prBranch = "goobers/impl/remediation-364"
	work := t.TempDir()
	gitCommand(t, work, "init", "--quiet", "-b", "main", work)
	writeFile(t, work, "conflict.txt", "original\n")
	gitCommand(t, work, "add", "conflict.txt")
	gitCommand(t, work, "commit", "--quiet", "-m", "seed")
	gitCommand(t, work, "checkout", "--quiet", "-b", prBranch)
	writeFile(t, work, "conflict.txt", "the PR's version\n")
	gitCommand(t, work, "commit", "--quiet", "-am", "PR edit")
	gitCommand(t, work, "checkout", "--quiet", "main")
	writeFile(t, work, "conflict.txt", "base's version\n")
	gitCommand(t, work, "commit", "--quiet", "-am", "base edit")
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitCommand(t, work, "clone", "--quiet", "--bare", work, bare)

	prev := checkoutCloneURL
	t.Cleanup(func() { checkoutCloneURL = prev })
	stageCheckoutEnv(t, bare, string(apiv1.WorkspaceRepo))
	t.Setenv(dispatcher.EnvWorkspaceBranch, prBranch)
	t.Setenv(dispatcher.EnvStageSyncBase, "true")
	t.Setenv(dispatcher.EnvStageCommand, `["true"]`)
	t.Setenv(dispatcher.EnvStageTimeout, "10s")
	t.Setenv(dispatcher.EnvDaemonAPI, "")

	ws := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(ws); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	got := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if got.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure", got.Status)
	}
	if got.Error == nil || got.Error.Code != runner.BaseSyncConflictErrorCode {
		t.Fatalf("error = %+v, want code %q (the self arms' classification, so failure-class routes to remediation instead of an implementation repass)", got.Error, runner.BaseSyncConflictErrorCode)
	}
	if !got.Error.Retryable {
		t.Fatal("a base_sync_conflict must be Retryable, matching internal/engine/activities.go's RunDeterministic and internal/runner/run.go — failure-class's isRecognizedInfrastructureFailure never matches this code, so Retryable is the only thing that routes it OutcomeInfra")
	}
}

// --- the plane environment through the in-pod strip (Goobers#3897) --------

// A goobers-CLI stage subprocess KEEPS its plane environment. Losing it is
// the silent failure the whole issue is about: every plane client selects its
// backend from os.Getenv, so a stripped GOOBERS_CLAIMS_ENDPOINT does not fail
// — it takes the local-file branch against a scratch volume nothing reads,
// and a claim that never reached the daemon reports success.
func TestCLIStageKeepsItsPlaneEnvironment(t *testing.T) {
	t.Setenv(dispatcher.EnvStageIsCLI, "true")
	t.Setenv(dispatcher.EnvStageEnvDefaultDeny, "")
	stamped := map[string]string{
		dispatcher.ClaimsEndpointEnv:    "http://daemon:7777",
		dispatcher.ClaimsTokenEnv:       "goobers-pod.claims",
		dispatcher.StateEndpointEnv:     "http://daemon:7777",
		dispatcher.StateTokenEnv:        "goobers-pod.state",
		dispatcher.JournalEndpointEnv:   "http://daemon:7777",
		dispatcher.JournalTokenEnv:      "goobers-pod.journal",
		dispatcher.TelemetryEndpointEnv: "http://daemon:7777",
		dispatcher.TelemetryTokenEnv:    "goobers-pod.telemetry",
		dispatcher.EnvRunID:             "run-1",
		dispatcher.EnvGaggle:            "alpha",
	}
	for name, value := range stamped {
		t.Setenv(name, value)
	}
	// The privileged half is present in the pod and must not survive.
	t.Setenv(dispatcher.EnvPodToken, "goobers-pod.surrender")
	t.Setenv(dispatcher.EnvDaemonAPI, "http://daemon:7777")

	got := envMap(stageEnvironment())
	for name, want := range stamped {
		if got[name] != want {
			t.Errorf("%s = %q, want %q; a CLI stage that loses this takes the local-file branch silently", name, got[name], want)
		}
	}
	for _, name := range dispatcher.DispatcherPrivilegedEnv {
		if _, present := got[name]; present {
			t.Errorf("%s survived into a CLI stage subprocess", name)
		}
	}
}

// Every OTHER stage loses the whole control plane, plane bearers included: a
// workflow-authored shell stage has no business holding a bearer for the
// claims ledger.
func TestOrdinaryStageLosesThePlaneEnvironment(t *testing.T) {
	t.Setenv(dispatcher.EnvStageIsCLI, "")
	t.Setenv(dispatcher.EnvStageEnvDefaultDeny, "")
	for _, name := range dispatcher.DispatcherControlEnv {
		t.Setenv(name, "present")
	}
	t.Setenv("STAGE_DECLARED_VAR", "kept")

	got := envMap(stageEnvironment())
	for _, name := range dispatcher.DispatcherControlEnv {
		if _, present := got[name]; present {
			t.Errorf("%s survived into an ordinary stage subprocess", name)
		}
	}
	if got["STAGE_DECLARED_VAR"] != "kept" {
		t.Error("the strip removed a stage's own declared variable")
	}
}

// The env:default-deny rebuild must not be a way back in. The allowlist
// re-admits by NAME and runs BEFORE the control strip, so a class enforcing
// default-deny is exactly where a re-admitted pod token would be easiest to
// miss — and exactly where a CLI stage's plane variables must still survive.
func TestEnvDefaultDenyRebuildKeepsPlaneEnvAndStillDropsThePodToken(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cli       string
		wantPlane bool
	}{
		{"goobers-CLI stage", "true", true},
		{"ordinary stage", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range dispatcher.DispatcherControlEnv {
				t.Setenv(name, "present")
			}
			allow, err := json.Marshal(append([]string{"STAGE_DECLARED_VAR"}, dispatcher.DispatcherControlEnv...))
			if err != nil {
				t.Fatal(err)
			}
			// Set last, and deliberately: these three ARE control variables
			// themselves (the dispatcher's own signals to the wrapper), so
			// the blanket loop above would otherwise clobber the very inputs
			// that decide which strip runs.
			//
			// The hostile case: the allowlist names the ENTIRE control plane.
			t.Setenv(dispatcher.EnvStageEnvDefaultDeny, "true")
			t.Setenv(dispatcher.EnvStageEnvAllow, string(allow))
			t.Setenv(dispatcher.EnvStageIsCLI, tc.cli)
			t.Setenv("STAGE_DECLARED_VAR", "kept")

			got := envMap(stageEnvironment())
			for _, name := range dispatcher.DispatcherPrivilegedEnv {
				if _, present := got[name]; present {
					t.Errorf("%s was re-admitted through the env:default-deny allowlist", name)
				}
			}
			for _, name := range dispatcher.DispatcherPlaneEnv {
				_, present := got[name]
				if present != tc.wantPlane {
					t.Errorf("%s present = %t, want %t", name, present, tc.wantPlane)
				}
			}
			if got["STAGE_DECLARED_VAR"] != "kept" {
				t.Error("the allowlist dropped a stage's own declared variable")
			}
		})
	}
}

func envMap(env []string) map[string]string {
	got := make(map[string]string, len(env))
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		got[name] = value
	}
	return got
}
