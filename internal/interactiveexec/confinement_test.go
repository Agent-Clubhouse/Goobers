//go:build !windows

package interactiveexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/sandbox"
)

func TestNativeSandboxUnavailableFailsClosed(t *testing.T) {
	e, req := bridgeExecutor(t)
	e.config.Sandbox = nil
	e.newSandbox = func() (sandbox.Sandbox, error) { return nil, sandbox.ErrUnavailable }
	started := false
	e.chatRegistry = nil
	e.runNative = func(_ context.Context, _ harness.RunRequest) (harness.Outcome, error) {
		started = true
		return successOutcome(), nil
	}
	_, err := e.native(t.Context(), req, t.TempDir(), "", func(string, map[string]any) error { return nil })
	if !errors.Is(err, harness.ErrSandboxUnavailable) || started {
		t.Fatal("missing sandbox did not prevent native execution", err)
	}
}

func TestNativeSandboxDeniesHumanSecretAndOutsideWrites(t *testing.T) {
	base, err := sandbox.New()
	if err != nil {
		if errors.Is(err, sandbox.ErrUnavailable) || errors.Is(err, sandbox.ErrUnsupported) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	e, _ := bridgeExecutor(t)
	e.config.Sandbox = base
	secret := filepath.Join(e.config.StateDir, "human-principal-secret")
	if err := os.WriteFile(secret, []byte("fixture-human-authority"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, err := e.workspace("sandbox-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	boundary, err := e.nativeSandbox()
	if err != nil {
		t.Fatal(err)
	}
	// The subprocess must be able to write its workspace, while kernel policy
	// prevents reading the human credential or writing another host directory.
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c",
		`if cat "$1" >/dev/null 2>&1; then exit 11; fi; if (echo bad >"$2") 2>/dev/null; then exit 12; fi; echo allowed >inside`,
		"fixture", secret, outside)
	command.Dir = workspace
	if err := boundary.Wrap(command, sandbox.Policy{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("confinement proof failed: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(workspace, "inside")); err != nil {
		t.Fatal("workspace writes unexpectedly denied", err)
	}
}
