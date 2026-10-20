//go:build integration && (darwin || linux)

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sandbox"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This exercises the real OS wrapper and HTTP boundary, with no model calls.
func TestIntegrationHumanControlRejectsConfinedAgent(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_AUTH_SANDBOX")
	boundary, err := sandbox.New()
	if err != nil {
		t.Fatal(err)
	}
	a := testHumanAuth(t)
	var effects atomic.Int32
	server := httptest.NewUnstartedServer(nil)
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = authenticatedPrototypeHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		effects.Add(1)
		w.WriteHeader(204)
	}), host, port, a)
	server.Start()
	defer server.Close()
	data, _ := json.Marshal(map[string]string{"token": a.bootstrap})
	bootstrap := httptest.NewRequest("POST", server.URL+"/auth/exchange", bytes.NewReader(data))
	bootstrap.Header.Set("Origin", server.URL)
	bootstrap.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(response, bootstrap)
	if response.Code != 200 {
		t.Fatalf("bootstrap status=%d", response.Code)
	}
	// Simulate a human browser cookie capture. Production keeps the bootstrap
	// link in denied host state and session secrets in host memory.
	privateState := t.TempDir()
	secretFile := filepath.Join(privateState, "human-browser-state.json")
	if err := os.WriteFile(secretFile, []byte(a.session+"\n"+a.csrf), 0600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, self, "-test.run=^TestIntegrationHumanControlConfinedChild$", "-test.v")
	child.Dir = workspace
	child.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + workspace, "TMPDIR=" + workspace,
		"GOOBERS_HUMAN_AUTH_CHILD=1", "CONTROL_URL=" + server.URL, "CONTROL_SECRET_PATH=" + secretFile}
	if err := boundary.Wrap(child, sandbox.Policy{Workspace: workspace, ReadDeniedPaths: []string{privateState}}); err != nil {
		t.Fatal(err)
	}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("confined agent probe failed: %v\n%s", err, output)
	}
	if effects.Load() != 0 {
		t.Fatal("confined agent reached a human control effect")
	}
}

func TestIntegrationHumanControlConfinedChild(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_HUMAN_AUTH_CHILD")
	if _, err := os.ReadFile(os.Getenv("CONTROL_SECRET_PATH")); err == nil {
		t.Fatal("agent could read human control secret from denied host state")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	// Prove the HTTP endpoint is reachable, so a network failure cannot mask a
	// missing authentication check.
	res, err := client.Get(os.Getenv("CONTROL_URL") + "/auth")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("public bootstrap page status=%d", res.StatusCode)
	}
	for _, path := range []string{"/sessions/s/commands", "/operations/actions/a/apply?sessionId=s"} {
		r, err := http.NewRequest("POST", os.Getenv("CONTROL_URL")+api+path, strings.NewReader(`{"type":"child.review","decision":"approve","expectedSequence":1,"expectedFence":1}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "goobers-local-actor", Value: "alice"})
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("agent control request status=%d, want 401", res.StatusCode)
		}
	}
}
