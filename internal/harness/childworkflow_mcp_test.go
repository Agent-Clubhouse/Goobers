package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/platform/secfile"
)

func childHarnessRequest(t *testing.T) RunRequest {
	t.Helper()
	ws := t.TempDir()
	return RunRequest{Workspace: ws, Envelope: testEnvelope(ws), ChildWorkflows: &mcpio.ChildWorkflowAccess{Endpoint: "https://daemon.invalid", BearerToken: "goobers-child.test.secret-signature"}}
}

func TestChildWorkflowToolsOnlyForTrustedHarnessRequest(t *testing.T) {
	req := childHarnessRequest(t)
	for _, adapter := range []struct {
		name, prefix string
		prepare      func(RunRequest, string) RunRequest
	}{
		{"copilot", goobersIOServerName + "-", withAutoGoobersIO},
		{"claude", "mcp__" + goobersIOServerName + "__", withAutoGoobersIOClaude},
	} {
		t.Run(adapter.name, func(t *testing.T) {
			prepared := adapter.prepare(req, "goobers")
			for _, name := range mcpio.ChildWorkflowToolNames(req.ChildWorkflows, req.Envelope.RunID) {
				if !slices.Contains(prepared.Tools, adapter.prefix+name) {
					t.Fatalf("missing %s", name)
				}
			}
			if len(req.Tools) != 0 {
				t.Fatal("modified caller's allowlist")
			}
			for _, unavailable := range []RunRequest{func() RunRequest { r := req; r.ChildWorkflows = nil; return r }(), func() RunRequest { r := req; r.Envelope.RunID = ""; return r }()} {
				prepared = adapter.prepare(unavailable, "goobers")
				for _, tool := range prepared.Tools {
					if strings.Contains(tool, "child_workflow") {
						t.Fatal("ungranted tool added")
					}
				}
			}
			if got := adapter.prepare(req, ""); len(got.Tools) != 0 {
				t.Fatal("tool granted without registered server")
			}
		})
	}
}

func TestChildWorkflowRuntimeRegistrationKeepsGrantPrivate(t *testing.T) {
	for _, adapter := range []string{"copilot", "claude", "codex"} {
		t.Run(adapter, func(t *testing.T) {
			req := childHarnessRequest(t)
			var registration string
			switch adapter {
			case "copilot":
				path, err := goobersIOAdditionalMCPConfigArg(req, "goobers")
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				registration = string(data)
				for _, name := range mcpio.ChildWorkflowToolNames(req.ChildWorkflows, req.Envelope.RunID) {
					if !strings.Contains(registration, name) {
						t.Fatalf("missing registration %s", name)
					}
				}
			case "claude":
				var err error
				registration, err = goobersIOClaudeMCPConfigArg(req, "goobers")
				if err != nil {
					t.Fatal(err)
				}
			case "codex":
				req = withAutoGoobersIOCodex(req, "goobers")
				_, _, overrides, err := prepareCodexMCP(context.Background(), req, t.TempDir(), "goobers", nil, nil, true)
				if err != nil {
					t.Fatal(err)
				}
				registration = strings.Join(overrides, "\n")
				if !strings.Contains(registration, "enabled_tools=") || !strings.Contains(registration, "start_child_workflow") {
					t.Fatal("Codex tool allowlist missing")
				}
			}
			if strings.Contains(registration, req.ChildWorkflows.BearerToken) || strings.Contains(registration, req.ChildWorkflows.Endpoint) {
				t.Fatal("registration argv exposes access")
			}
			configPath := filepath.Join(req.Workspace, filepath.FromSlash(goobersIORuntimeSubdir), mcpio.ConfigFileName)
			if err := secfile.VerifyPrivate(configPath); err != nil {
				t.Fatalf("private runtime config err=%v", err)
			}
			cfg, err := mcpio.LoadConfig(configPath)
			if err != nil || cfg.ChildWorkflows == nil || cfg.ChildWorkflows.BearerToken != req.ChildWorkflows.BearerToken {
				t.Fatal("trusted grant not delivered")
			}
			req.GoobersIORegistered = true
			prompt := renderPrompt(req)
			for _, name := range []string{"validate_child_workflow", "start_child_workflow", "get_child_workflow"} {
				if !strings.Contains(prompt, name) {
					t.Fatalf("prompt omits %s", name)
				}
			}
			if strings.Contains(prompt, req.ChildWorkflows.BearerToken) || strings.Contains(prompt, req.ChildWorkflows.Endpoint) {
				t.Fatal("prompt exposes access")
			}
			req.ChildWorkflows = nil
			if strings.Contains(renderPrompt(req), "start_child_workflow") {
				t.Fatal("ordinary prompt mentions child tools")
			}
			if _, _, err := prepareGoobersIOMCPRuntime(req, "goobers"); err != nil {
				t.Fatal(err)
			}
			cfg, err = mcpio.LoadConfig(configPath)
			if err != nil || cfg.ChildWorkflows != nil {
				t.Fatal("ordinary next attempt retained stale grant")
			}
		})
	}
}
