//go:build windows

package executor

import (
	"os/exec"
	"strings"
	"testing"
)

func TestConfigureNoNetworkFailsClosedOnWindows(t *testing.T) {
	t.Setenv(allowUnisolatedNetworkNoneEnv, "")

	err := configureNoNetwork(exec.Command("cmd.exe"))
	if err == nil || !strings.Contains(err.Error(), allowUnisolatedNetworkNoneEnv) {
		t.Fatalf("configureNoNetwork() error = %v, want explicit trusted-local opt-in guidance", err)
	}
}

func TestConfigureNoNetworkAllowsExplicitTrustedLocalOptIn(t *testing.T) {
	t.Setenv(allowUnisolatedNetworkNoneEnv, "1")
	command := exec.Command("cmd.exe")

	if err := configureNoNetwork(command); err != nil {
		t.Fatalf("configureNoNetwork() error = %v", err)
	}
	if got := command.Env[len(command.Env)-1]; got != "GOOBERS_NETWORK_ISOLATION=unsupported-windows" {
		t.Fatalf("network isolation marker = %q", got)
	}
}
