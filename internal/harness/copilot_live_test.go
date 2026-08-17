//go:build integration

package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func liveCopilotCredentials(t *testing.T) *credentials.Set {
	t.Helper()
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, nil, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	creds, err := injector.Materialize(context.Background(), nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return creds
}

func TestIntegrationCopilotAdapterLiveSmoke(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_COPILOT_LIVE_SMOKE")
	testdep.Require(t, "copilot")

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "GREETING.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("seed fixture file: %v", err)
	}

	adapter := &CopilotAdapter{Command: []string{"copilot"}}
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	env := testEnvelope(workspace)
	env.Goal = "Append the word 'world' to GREETING.txt, then write your result envelope as instructed."
	req := RunRequest{
		Mode:           ModeInvoke,
		Envelope:       env,
		Instructions:   "You are a coder goober performing a trivial smoke-test edit.",
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    liveCopilotCredentials(t),
		Timeout:        2 * time.Minute,
	}
	out, err := adapter.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v (transcript: %s)", err, out.Transcript)
	}
	if len(out.Payload) == 0 {
		t.Fatal("expected a completion payload from the live CLI")
	}
}

func TestIntegrationCopilotAdapterToolAllowlistRejectsOmittedMutation(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_COPILOT_LIVE_SMOKE")
	testdep.Require(t, "copilot")

	adapter := &CopilotAdapter{Command: []string{"copilot"}}
	creds := liveCopilotCredentials(t)
	const goal = "Use the create tool to create TOOL_CONTROL.txt containing exactly 'available'. " +
		"Do not use another tool or method to create it. If the create tool is unavailable, return status failure " +
		"with error code TOOL_UNAVAILABLE; otherwise return status success."

	run := func(t *testing.T, tools []string) (string, apiv1.ResultEnvelope) {
		t.Helper()
		workspace := t.TempDir()
		env := testEnvelope(workspace)
		env.Goal = goal
		out, err := adapter.Run(context.Background(), RunRequest{
			Mode:           ModeInvoke,
			Envelope:       env,
			Tools:          tools,
			Workspace:      workspace,
			CompletionPath: DefaultResultPath,
			Credentials:    creds,
			Timeout:        2 * time.Minute,
		})
		if err != nil {
			t.Fatalf("Run: %v (transcript: %s)", err, out.Transcript)
		}
		var completion apiv1.ResultEnvelope
		if err := json.Unmarshal(out.Payload, &completion); err != nil {
			t.Fatalf("decode completion: %v", err)
		}
		return workspace, completion
	}

	t.Run("allowed control", func(t *testing.T) {
		workspace, completion := run(t, []string{"create"})
		if completion.Status != apiv1.ResultSuccess {
			t.Fatalf("allowed create tool returned status %q", completion.Status)
		}
		content, err := os.ReadFile(filepath.Join(workspace, "TOOL_CONTROL.txt"))
		if err != nil {
			t.Fatalf("read tool control file: %v", err)
		}
		if strings.TrimSpace(string(content)) != "available" {
			t.Fatalf("tool control content = %q, want %q", content, "available")
		}
	})

	t.Run("omitted negative control", func(t *testing.T) {
		workspace, completion := run(t, []string{"view"})
		if completion.Status != apiv1.ResultFailure || completion.Error == nil ||
			completion.Error.Code != "TOOL_UNAVAILABLE" {
			t.Fatalf("omitted create tool completion = %#v, want failure TOOL_UNAVAILABLE", completion)
		}
		if _, err := os.Stat(filepath.Join(workspace, "TOOL_CONTROL.txt")); !os.IsNotExist(err) {
			t.Fatalf("omitted create tool created TOOL_CONTROL.txt: %v", err)
		}
	})
}
