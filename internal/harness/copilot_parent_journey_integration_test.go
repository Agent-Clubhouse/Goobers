//go:build integration && !windows

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract/childworkflowwire"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This subprocess is the real stdio MCP server, launched from the SDK's actual
// registration. The test endpoint substitutes daemon execution, not MCP tools.
func TestIntegrationCopilotQualificationMCPProcess(t *testing.T) {
	testdep.Require(t, "sh")
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	if len(args) != 3 || args[0] != "mcp-io" || args[1] != "--config" {
		os.Exit(2)
	}
	cfg, err := mcpio.LoadConfig(args[2])
	if err != nil {
		os.Exit(3)
	}
	if err := mcpio.NewServer(mcpio.NewToolset(cfg)).Serve(os.Stdin, os.Stdout, io.Discard); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

// Exercise three fresh SDK sessions: author/validate/start, resume/resolve,
// then acknowledge/complete. Only the CLI/model and daemon responses are fake.
// The contained-pod journeys separately exercise the real coordinator/backend.
func TestIntegrationCopilotParentSDKAuthoringJourney(t *testing.T) {
	testdep.Require(t, "node", "sh")
	workspace := t.TempDir()
	fixture, err := filepath.Abs("../../cmd/goobers/testdata/qualification-copilot.cjs")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(t.TempDir(), "goobers")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestIntegrationCopilotQualificationMCPProcess$ -- \"$@\"\n"
	if err := os.WriteFile(self, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	seedCopilotAuthoringJourney(t, workspace)
	endpoint, advance, verify := copilotAuthoringDaemon(t, workspace)
	defer verify()
	command := filepath.Join(t.TempDir(), "copilot")
	launcher := "#!/bin/sh\nexec node '" + strings.ReplaceAll(fixture, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(command, []byte(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	adapter := &CopilotAdapter{Command: []string{command}, SelfBin: self, EnvCapabilities: map[string]string{"agent:model": "COPILOT_GITHUB_TOKEN"}}
	req := RunRequest{
		Envelope: testEnvelope(workspace, "agent:model"), Workspace: workspace,
		Credentials:           mcpTestCredentials(t, "agent:model", "qualification-copilot-model-token"),
		HarnessConfigResolved: true, HarnessVersion: "1.0.98", CompletionPath: DefaultResultPath,
		Instructions:   "Read .github/skills/goobers-child-workflows/SKILL.md and author the requested child.",
		ChildWorkflows: &mcpio.ChildWorkflowAccess{Endpoint: endpoint, BearerToken: "goobers-child.qualification"},
		Timeout:        15 * time.Second,
	}
	for attempt := 0; attempt < 3; attempt++ {
		req.Attempt = attempt + 1
		var readiness MCPReadiness
		req.MCPReadinessSink = func(report MCPReadiness) error { readiness = report; return nil }
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		type result struct {
			out Outcome
			err error
		}
		done := make(chan result, 1)
		go func() { out, err := adapter.Run(ctx, req); done <- result{out, err} }()
		if attempt < 2 {
			select {
			case <-advance:
				cancel()
			case got := <-done:
				cancel()
				t.Fatalf("attempt %d completed before yield: %v transcript=%s", attempt, got.err, got.out.Transcript)
			case <-ctx.Done():
				cancel()
				t.Fatalf("attempt %d did not reach child mutation", attempt)
			}
		}
		got := <-done
		cancel()
		// Cancellation can beat detached usage capture (#7650); only the
		// completed session currently guarantees its accounting snapshot.
		if readiness.Category != "ready" || attempt == 2 && got.out.Metrics[telemetry.AttrUsageNanoAIU] != 1e9 {
			t.Fatalf("attempt %d lost readiness or usage: %+v metrics=%v", attempt, readiness, got.out.Metrics)
		}
		if attempt < 2 {
			if !errors.Is(got.err, ErrCanceled) || len(got.out.Payload) != 0 {
				t.Fatalf("attempt %d did not yield: %v payload=%s", attempt, got.err, got.out.Payload)
			}
		} else if got.err != nil || !strings.Contains(string(got.out.Payload), "Generated child completed and disposition acknowledged") {
			t.Fatalf("response completion failed: %v payload=%s transcript=%s", got.err, got.out.Payload, got.out.Transcript)
		}
		for _, secret := range []string{req.ChildWorkflows.BearerToken, "qualification-copilot-model-token"} {
			if strings.Contains(string(got.out.Transcript), secret) {
				t.Fatal("credential exposed in transcript")
			}
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, DefaultResultPath)); !os.IsNotExist(err) {
		t.Fatal("Copilot completion must come from its final response")
	}
}

func seedCopilotAuthoringJourney(t *testing.T, workspace string) {
	t.Helper()
	skill, err := os.ReadFile("../childworkflow/authoring/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(workspace, ".github", "skills", "goobers-child-workflows")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	catalog := `{"schemaVersion":"goobers-child-authoring/v1","newSubmissionsAvailable":true,"dslVersion":"3.1","gaggle":"example","maxStates":128,"goobers":[{"name":"coder","harness":"copilot"}],"runners":[{"os":"linux","shell":true,"capabilities":["isolated-child"]}]}`
	for path, content := range map[string]string{filepath.Join(directory, "SKILL.md"): string(skill), filepath.Join(directory, "catalog.json"): catalog, filepath.Join(workspace, "qualification-mode"): "merge"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func copilotAuthoringDaemon(t *testing.T, workspace string) (string, <-chan struct{}, func()) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	var source string
	state := 0
	advance := make(chan struct{}, 2)
	ref := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer goobers-child.qualification" {
			t.Error("launcher authority lost")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		path := strings.ReplaceAll(r.URL.Path, "run-1", "{run}")
		calls = append(calls, path)
		w.Header().Set("Content-Type", "application/json")
		switch path {
		case childworkflowwire.StatusPath:
			if body["invocationKey"] != "qualification-child" {
				t.Error("status identity changed")
			}
			if state == 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"code":"child_workflow_not_found"}}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"state":"completed","resultRef":%q,"acknowledged":%t}`, ref, state == 2)
		case childworkflowwire.ValidatePath:
			source = body["source"]
			var document map[string]any
			if err := json.Unmarshal([]byte(source), &document); err != nil || document["dslVersion"] != "3.1" || !strings.Contains(source, "parent-before-child.txt") {
				t.Error("authored workflow differs")
			}
			_, _ = io.WriteString(w, `{"valid":true}`)
		case childworkflowwire.StartPath:
			if state != 0 || source == "" || source != body["source"] || r.Header.Get("Idempotency-Key") != "qualification-child" {
				t.Error("accepted source or identity changed")
			}
			state = 1
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"state":"queued"}`)
			advance <- struct{}{}
		case childworkflowwire.ResolvePath:
			if state != 1 || body["invocationKey"] != "qualification-child" || body["action"] != "merge" || body["resultRef"] != ref {
				t.Error("result disposition changed")
			}
			// Substitute the backend's merge; the production worker tests must prove it.
			if err := os.WriteFile(filepath.Join(workspace, "child-return.txt"), []byte("child return\n"), 0600); err != nil {
				t.Error(err)
			}
			state = 2
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"applied":false}`)
			advance <- struct{}{}
		default:
			t.Error("unexpected child route")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return server.URL, advance, func() {
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		want := []string{childworkflowwire.StatusPath, childworkflowwire.ValidatePath, childworkflowwire.StartPath, childworkflowwire.StatusPath, childworkflowwire.ResolvePath, childworkflowwire.StatusPath}
		if !slices.Equal(calls, want) || state != 2 {
			t.Errorf("child journey calls=%v state=%d", calls, state)
		}
	}
}
