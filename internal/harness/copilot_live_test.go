//go:build integration

package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/credentials"
)

func TestCopilotAdapterLiveSmoke(t *testing.T) {
	if os.Getenv("GOOBERS_COPILOT_LIVE_SMOKE") != "1" {
		t.Skip("set GOOBERS_COPILOT_LIVE_SMOKE=1 to run against a real, signed-in Copilot CLI")
	}
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skip("copilot CLI not found on PATH")
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "GREETING.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("seed fixture file: %v", err)
	}

	adapter := &CopilotAdapter{Command: []string{"copilot"}}
	if err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}

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

	env := testEnvelope(workspace)
	env.Goal = "Append the word 'world' to GREETING.txt, then write your result envelope as instructed."
	req := RunRequest{
		Mode:           ModeInvoke,
		Envelope:       env,
		Instructions:   "You are a coder goober performing a trivial smoke-test edit.",
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
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
