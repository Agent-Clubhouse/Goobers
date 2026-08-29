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
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
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

// The two halves must remain a PARTITION of the old single list. A name that
// falls out of both is a variable that silently stops being stripped for every
// stage — the failure this split could plausibly introduce.
func TestDispatcherControlEnvIsExactlyItsTwoHalves(t *testing.T) {
	union := map[string]bool{}
	for _, n := range dispatcher.DispatcherPrivilegedEnv {
		union[n] = true
	}
	for _, n := range dispatcher.DispatcherRunIdentityEnv {
		if union[n] {
			t.Fatalf("%q is in BOTH halves; the split must be a partition", n)
		}
		union[n] = true
	}
	if len(union) != len(dispatcher.DispatcherControlEnv) {
		t.Fatalf("halves cover %d names, control plane has %d", len(union), len(dispatcher.DispatcherControlEnv))
	}
	for _, n := range dispatcher.DispatcherControlEnv {
		if !union[n] {
			t.Fatalf("%q is in neither half: it would stop being stripped", n)
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
