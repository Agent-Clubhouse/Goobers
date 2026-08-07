package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func newTestGuidedServer(t *testing.T, workdir string) *guidedServer {
	t.Helper()
	return &guidedServer{
		workdir:      workdir,
		samplePath:   filepath.Join(workdir, gettingStartedSampleDirName),
		instancePath: filepath.Join(workdir, gettingStartedInstanceDirName),
		executable:   "goobers-under-test",
		errorLog:     log.New(io.Discard, "", 0),
	}
}

// stubGuidedExec replaces the subprocess seam, recording each action's argv and
// running the given shell script in its place.
func stubGuidedExec(t *testing.T, script string) *[][]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("guided exec stubs use /bin/sh")
	}
	previous := guidedExecCommand
	calls := &[][]string{}
	guidedExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "goobers-under-test" {
			t.Errorf("guided exec name = %q, want the server's executable", name)
		}
		*calls = append(*calls, append([]string{}, args...))
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
	t.Cleanup(func() { guidedExecCommand = previous })
	return calls
}

func guidedGet(handler http.Handler, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func guidedPost(handler http.Handler, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeGuidedResponse[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode guided response %q: %v", recorder.Body.String(), err)
	}
	return out
}

func TestGettingStartedStateEndpoint(t *testing.T) {
	workdir := t.TempDir()
	server := newTestGuidedServer(t, workdir)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "")
	t.Setenv("GOOBERS_GITHUB_ISSUES_TOKEN", "")

	before := guidedGet(http.HandlerFunc(server.serveGuided), "/guided/state")
	if before.Code != http.StatusOK {
		t.Fatalf("state status = %d body = %q", before.Code, before.Body.String())
	}
	if contentType := before.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("state Content-Type = %q", contentType)
	}
	state := decodeGuidedResponse[guidedStateBody](t, before)
	if state.Version != 1 ||
		state.Workdir != workdir ||
		state.SamplePath != filepath.Join(workdir, "getting-started-task-api") ||
		state.InstancePath != filepath.Join(workdir, "tutorial-instance") {
		t.Fatalf("state paths = %+v", state)
	}
	if state.SampleExists || state.InstanceExists || state.APIReady || state.Job != nil {
		t.Fatalf("fresh workdir state = %+v", state)
	}
	if state.Env.GoobersGithubToken || state.Env.GoobersGithubIssuesToken {
		t.Fatalf("empty env reported set: %+v", state.Env)
	}
	// The raw body must say "job":null explicitly, not omit it.
	if !strings.Contains(before.Body.String(), `"job":null`) {
		t.Fatalf("state body missing explicit null job: %q", before.Body.String())
	}

	if err := os.MkdirAll(server.samplePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(server.instancePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server.instancePath, "instance.yaml"), []byte("x: y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_GITHUB_TOKEN", "token-value")
	t.Setenv("GOOBERS_GITHUB_ISSUES_TOKEN", "issues-token")

	after := decodeGuidedResponse[guidedStateBody](t, guidedGet(http.HandlerFunc(server.serveGuided), "/guided/state"))
	if !after.SampleExists || !after.InstanceExists {
		t.Fatalf("state after creation = %+v", after)
	}
	if !after.Env.GoobersGithubToken || !after.Env.GoobersGithubIssuesToken {
		t.Fatalf("set env reported unset: %+v", after.Env)
	}
}

func TestGettingStartedStateNeverLeaksTokenValues(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	t.Setenv("GOOBERS_GITHUB_TOKEN", "super-secret-value")
	recorder := guidedGet(http.HandlerFunc(server.serveGuided), "/guided/state")
	if strings.Contains(recorder.Body.String(), "super-secret-value") {
		t.Fatalf("state leaked a token value: %q", recorder.Body.String())
	}
}

func TestGettingStartedActionArgv(t *testing.T) {
	workdir := t.TempDir()
	sample := filepath.Join(workdir, "getting-started-task-api")
	tutorial := filepath.Join(workdir, "tutorial-instance")
	cases := []struct {
		name   string
		invoke func(handler http.Handler) *httptest.ResponseRecorder
		want   []string
	}{
		{
			name: "stub-sample default",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/stub-sample", `{}`)
			},
			want: []string{"onboarding", "stub-sample", "--destination", sample, "--json"},
		},
		{
			name: "stub-sample all options",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/stub-sample",
					`{"workTracking":"my-org/tutorial","tokenEnv":"MY_TOKEN","force":true}`)
			},
			want: []string{
				"onboarding", "stub-sample", "--destination", sample, "--json",
				"--work-tracking", "my-org/tutorial", "--token-env", "MY_TOKEN", "--force",
			},
		},
		{
			name: "init-instance",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/init-instance", `{}`)
			},
			want: []string{"init", "--template=quickstart", tutorial},
		},
		{
			name: "validate default",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/validate", `{"checkHarness":false,"checkRepos":false}`)
			},
			want: []string{"validate", "--json", tutorial},
		},
		{
			name: "validate with checks",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/validate", `{"checkHarness":true,"checkRepos":true}`)
			},
			want: []string{"validate", "--json", "--check-harness", "--check-repos", tutorial},
		},
		{
			name: "status",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedGet(h, "/guided/status")
			},
			want: []string{"status", "--json", tutorial},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestGuidedServer(t, workdir)
			calls := stubGuidedExec(t, `printf '{"action":"stub","version":1}'`)
			recorder := testCase.invoke(http.HandlerFunc(server.serveGuided))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
			}
			if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], testCase.want) {
				t.Fatalf("argv = %v, want [%v]", *calls, testCase.want)
			}
			body := decodeGuidedResponse[map[string]any](t, recorder)
			if _, hasExit := body["exitCode"]; !hasExit {
				t.Fatalf("response missing exitCode: %v", body)
			}
		})
	}
}

func TestGettingStartedEnvelopeParsingAndExitCode(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)

	stubGuidedExec(t, `printf '{"action":"validate.report","version":1}'; printf 'warned\n' >&2; exit 1`)
	envelope := decodeGuidedResponse[guidedEnvelopeBody](t, guidedPost(handler, "/guided/actions/validate", `{}`))
	if envelope.ExitCode != 1 {
		t.Fatalf("exitCode = %d", envelope.ExitCode)
	}
	if string(envelope.Envelope) != `{"action":"validate.report","version":1}` {
		t.Fatalf("envelope = %s", envelope.Envelope)
	}
	if envelope.Stderr != "warned\n" {
		t.Fatalf("stderr = %q", envelope.Stderr)
	}

	stubGuidedExec(t, `printf 'not json at all\n'; exit 2`)
	unparsed := decodeGuidedResponse[guidedEnvelopeBody](t, guidedPost(handler, "/guided/actions/validate", `{}`))
	if unparsed.ExitCode != 2 || string(unparsed.Envelope) != "null" {
		t.Fatalf("unparseable stdout: exit = %d envelope = %s", unparsed.ExitCode, unparsed.Envelope)
	}
}

func TestGettingStartedGuidedRefusesCrossOrigin(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)

	crossOrigin := httptest.NewRequest(http.MethodGet, "/guided/state", nil)
	crossOrigin.Host = "127.0.0.1:8081"
	crossOrigin.Header.Set("Origin", "http://attacker.example")
	refused := httptest.NewRecorder()
	handler.ServeHTTP(refused, crossOrigin)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d body = %q", refused.Code, refused.Body.String())
	}
	if body := decodeGuidedResponse[guidedErrorBody](t, refused); body.Code != "origin_forbidden" {
		t.Fatalf("cross-origin code = %q", body.Code)
	}

	// Non-loopback Origin is refused even when it matches the request host.
	nonLoopback := httptest.NewRequest(http.MethodGet, "/guided/state", nil)
	nonLoopback.Host = "portal.example:8081"
	nonLoopback.Header.Set("Origin", "http://portal.example:8081")
	refusedNonLoopback := httptest.NewRecorder()
	handler.ServeHTTP(refusedNonLoopback, nonLoopback)
	if refusedNonLoopback.Code != http.StatusForbidden {
		t.Fatalf("non-loopback status = %d", refusedNonLoopback.Code)
	}

	matching := httptest.NewRequest(http.MethodGet, "/guided/state", nil)
	matching.Host = "127.0.0.1:8081"
	matching.Header.Set("Origin", "http://127.0.0.1:8081")
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, matching)
	if allowed.Code != http.StatusOK {
		t.Fatalf("loopback same-origin status = %d body = %q", allowed.Code, allowed.Body.String())
	}
}

func TestGettingStartedPostRequiresJSONContentType(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)
	calls := stubGuidedExec(t, `printf '{}'`)

	request := httptest.NewRequest(http.MethodPost, "/guided/actions/validate", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "text/plain")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type refusal status = %d", recorder.Code)
	}
	if len(*calls) != 0 {
		t.Fatalf("refused request still exec'd: %v", *calls)
	}
}

func TestGettingStartedPostRejectsUnknownFields(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)
	calls := stubGuidedExec(t, `printf '{}'`)

	recorder := guidedPost(handler, "/guided/actions/validate", `{"checkHarness":true,"surprise":1}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("refused request still exec'd: %v", *calls)
	}
}

func TestGettingStartedJobLifecycle(t *testing.T) {
	workdir := t.TempDir()
	server := newTestGuidedServer(t, workdir)
	handler := http.HandlerFunc(server.serveGuided)
	calls := stubGuidedExec(t,
		`printf 'created run abc123 (workflow=quickstart gaggle=example)\n'; printf 'stage research started\n' >&2; exit 3`)

	accepted := guidedPost(handler, "/guided/actions/run", `{}`)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("run status = %d body = %q", accepted.Code, accepted.Body.String())
	}
	jobID := decodeGuidedResponse[map[string]string](t, accepted)["jobId"]
	if jobID == "" {
		t.Fatal("run returned no jobId")
	}
	if want := []string{"run", "quickstart", filepath.Join(workdir, "tutorial-instance")}; !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("run argv = %v, want %v", (*calls)[0], want)
	}

	deadline := time.Now().Add(5 * time.Second)
	var detail guidedJobDetail
	for {
		detail = decodeGuidedResponse[guidedJobDetail](t, guidedGet(handler, "/guided/jobs/"+jobID))
		if detail.Done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !detail.Done {
		t.Fatalf("job never finished: %+v", detail)
	}
	if detail.ID != jobID || detail.Kind != "run" {
		t.Fatalf("job identity = %+v", detail)
	}
	if detail.ExitCode == nil || *detail.ExitCode != 3 {
		t.Fatalf("job exitCode = %v", detail.ExitCode)
	}
	if detail.RunID == nil || *detail.RunID != "abc123" {
		t.Fatalf("job runId = %v", detail.RunID)
	}
	output := strings.Join(detail.Output, "\n")
	if !strings.Contains(output, "created run abc123") || !strings.Contains(output, "stage research started") {
		t.Fatalf("job output missing interleaved lines: %q", output)
	}

	// The state endpoint reflects the finished job.
	state := decodeGuidedResponse[guidedStateBody](t, guidedGet(handler, "/guided/state"))
	if state.Job == nil || state.Job.ID != jobID || !state.Job.Done {
		t.Fatalf("state job = %+v", state.Job)
	}

	if missing := guidedGet(handler, "/guided/jobs/nope"); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown job status = %d", missing.Code)
	}
}

func TestGettingStartedSecondRunWhileRunningConflicts(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)
	stubGuidedExec(t, `sleep 5`)

	first := guidedPost(handler, "/guided/actions/run", `{}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first run status = %d", first.Code)
	}
	second := guidedPost(handler, "/guided/actions/run", `{}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("second run status = %d body = %q", second.Code, second.Body.String())
	}
	if body := decodeGuidedResponse[guidedErrorBody](t, second); body.Code != "job_running" {
		t.Fatalf("conflict code = %q", body.Code)
	}
	if err := server.close(); err != nil {
		t.Fatal(err)
	}
}

func TestGettingStartedAPIUnavailableThenReady(t *testing.T) {
	workdir := t.TempDir()
	server := newTestGuidedServer(t, workdir)
	t.Cleanup(func() { _ = server.close() })
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(dashboardTestIndex)}}
	handler, err := newGettingStartedHandler(assets, server)
	if err != nil {
		t.Fatal(err)
	}

	before := guidedGet(handler, "/api/v1/health")
	if before.Code != http.StatusServiceUnavailable {
		t.Fatalf("pre-instance API status = %d body = %q", before.Code, before.Body.String())
	}
	if body := decodeGuidedResponse[guidedErrorBody](t, before); body.Code != "guided_no_instance" ||
		body.Message != "initialize the tutorial instance first" {
		t.Fatalf("pre-instance API body = %+v", body)
	}

	if code, _, stderr := runArgs(t, "init", server.instancePath); code != 0 {
		t.Fatalf("init tutorial instance: code = %d stderr = %q", code, stderr)
	}
	createDeclaredSkillPackages(t, server.instancePath, "implement", "run-tests")

	after := guidedGet(handler, "/api/v1/health")
	if after.Code != http.StatusOK {
		t.Fatalf("post-instance API status = %d body = %q", after.Code, after.Body.String())
	}
	state := decodeGuidedResponse[guidedStateBody](t, guidedGet(handler, "/guided/state"))
	if !state.APIReady || !state.InstanceExists {
		t.Fatalf("state after API ready = %+v", state)
	}
}

func TestGettingStartedHandlerServesRewrittenIndexAndAssets(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(dashboardTestIndex)},
		"app.js":     &fstest.MapFile{Data: []byte("app")},
	}
	handler, err := newGettingStartedHandler(assets, server)
	if err != nil {
		t.Fatal(err)
	}

	index := guidedGet(handler, "/")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), `content="getting-started"`) {
		t.Fatalf("index response = %d %q", index.Code, index.Body.String())
	}
	if cache := index.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("index Cache-Control = %q", cache)
	}

	static := guidedGet(handler, "/app.js")
	if static.Code != http.StatusOK || static.Body.String() != "app" {
		t.Fatalf("static response = %d %q", static.Code, static.Body.String())
	}

	spa := guidedGet(handler, "/getting-started")
	if spa.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", spa.Code)
	}
}
