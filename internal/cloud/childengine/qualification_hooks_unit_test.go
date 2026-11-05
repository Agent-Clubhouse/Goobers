//go:build cloudintegration

package childengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
)

func TestQualificationHTTPBarrierAllowsRouterReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h := &qualificationHarness{t: t, handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Qualification", "committed")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("old epoch result"))
	})}
	b := h.InstallHTTPBarrier(func(r *http.Request) bool { return r.URL.Path == "/bind" })
	defer b.Release()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.serveHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodPost, "/bind", nil))
	}()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	replaced := make(chan struct{})
	go func() {
		h.routerMu.Lock()
		h.handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		h.routerMu.Unlock()
		close(replaced)
	}()
	select {
	case <-replaced:
	case <-ctx.Done():
		t.Fatal("post-route barrier prevented epoch replacement")
	}
	select {
	case <-done:
		t.Fatal("response escaped the barrier")
	default:
	}
	b.Release()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if response.Code != http.StatusAccepted || response.Header().Get("X-Qualification") != "committed" || response.Body.String() != "old epoch result" {
		t.Fatalf("lost original response: %v %s", response, response.Body)
	}
	next := httptest.NewRecorder()
	h.serveHTTP(next, httptest.NewRequestWithContext(ctx, http.MethodPost, "/bind", nil))
	if next.Code != http.StatusNoContent {
		t.Fatalf("successor response = %d", next.Code)
	}
}

func TestQualificationSerializedStageBarrierAndOutcome(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h := &qualificationHarness{t: t}
	server := httptest.NewServer(http.HandlerFunc(h.serveHTTP))
	defer server.Close()
	b := h.StageBarrier("child-running")
	defer b.Release()
	h.SetStageScript(qualificationStageScript{"child": {Barrier: "child-running", Result: &apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "scripted result"}}})
	raw, err := json.Marshal(qualificationWorkerConfig{Endpoint: server.URL, Script: h.stageScript})
	if err != nil {
		t.Fatal(err)
	}
	var cfg qualificationWorkerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	result := make(chan engine.DispatchStageResult, 1)
	failure := make(chan error, 1)
	go func() {
		got, err := (qualificationStages{cfg: cfg}).child(ctx, apiv1.InvocationEnvelope{}, apiv1.DeterministicRun{}, "", "")
		result <- got
		failure <- err
	}()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result:
		t.Fatal("child completed while its stage barrier was held")
	default:
	}
	b.Release()
	select {
	case got := <-result:
		if err := <-failure; err != nil || got.Summary != "scripted result" {
			t.Fatalf("scripted stage = %+v, %v", got, err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cfg.Script["child"] = qualificationStageStep{Error: "scripted failure"}
	if _, err := (qualificationStages{cfg: cfg}).child(ctx, apiv1.InvocationEnvelope{}, apiv1.DeterministicRun{}, "", ""); err == nil || err.Error() != "scripted failure" {
		t.Fatalf("scripted error = %v", err)
	}
}

func TestQualificationInstanceReloadReadsEditedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	h := &qualificationHarness{t: t, configFile: instance.NewLayout(root).ConfigFile()}
	before := h.reloadInstanceConfig(true)
	raw, err := os.ReadFile(h.configFile)
	if err != nil {
		t.Fatal(err)
	}
	// SelfIdentity is an instance policy input, outside the definition set.
	if strings.Contains(string(raw), "selfIdentity:") {
		t.Fatal("fixture unexpectedly defines selfIdentity")
	}
	raw = append(raw, []byte("\nselfIdentity: qualification-reloaded\n")...)
	if err := os.WriteFile(h.configFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	after := h.reloadInstanceConfig(false)
	if before.SelfIdentity == after.SelfIdentity || after.SelfIdentity != "qualification-reloaded" ||
		!before.EngineChildWorkflowsEnabled() || after.EngineChildWorkflowsEnabled() {
		t.Fatal("reload did not read edited instance policy and explicit test posture independently of old config")
	}
}
