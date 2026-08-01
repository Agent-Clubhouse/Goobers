//go:build windows

package executor

import (
	"os/exec"
	"strings"
	"testing"
)

func TestConfigureNoNetworkFailsClosedOnWindows(t *testing.T) {
	t.Setenv(allowUnisolatedNetworkNoneEnv, "")

	marker, err := configureNoNetwork(exec.Command("cmd.exe"))
	if err == nil || !strings.Contains(err.Error(), allowUnisolatedNetworkNoneEnv) {
		t.Fatalf("configureNoNetwork() error = %v, want explicit trusted-local opt-in guidance", err)
	}
	if marker != "" {
		t.Fatalf("marker = %q, want empty on a fail-closed refusal (nothing ran)", marker)
	}
}

func TestConfigureNoNetworkAllowsExplicitTrustedLocalOptIn(t *testing.T) {
	t.Setenv(allowUnisolatedNetworkNoneEnv, "1")
	command := exec.Command("cmd.exe")

	marker, err := configureNoNetwork(command)
	if err != nil {
		t.Fatalf("configureNoNetwork() error = %v", err)
	}
	if got := command.Env[len(command.Env)-1]; got != "GOOBERS_NETWORK_ISOLATION=unsupported-windows" {
		t.Fatalf("network isolation marker (child env) = %q", got)
	}
	// #2034: the same fact must also be visible OUTSIDE the child process's
	// own env — a non-empty return value is what the caller (ShellExecutor.Run)
	// stamps into the stage's journaled Outputs.
	if marker != "unsupported-windows" {
		t.Fatalf("marker (return value) = %q, want %q", marker, "unsupported-windows")
	}
}
