//go:build linux

package executor

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// TestConfigureNoNetworkAppliesNamespacesByDefault proves the escape hatch
// is opt-in only: with the env var unset, configureNoNetwork always attempts
// real isolation (#4267 must not change behavior on a capable host).
func TestConfigureNoNetworkAppliesNamespacesByDefault(t *testing.T) {
	t.Setenv(allowUnisolatedNetworkNoneEnv, "")

	cmd := exec.Command("/bin/true")
	marker, err := configureNoNetwork(cmd)
	if err != nil {
		t.Fatalf("configureNoNetwork() error = %v", err)
	}
	if marker != "" {
		t.Fatalf("marker = %q, want empty when isolation is actually applied", marker)
	}
	if cmd.SysProcAttr.Cloneflags&(syscall.CLONE_NEWUSER|syscall.CLONE_NEWNET) == 0 {
		t.Fatalf("Cloneflags = %v, want CLONE_NEWUSER|CLONE_NEWNET set", cmd.SysProcAttr.Cloneflags)
	}
}

// TestConfigureNoNetworkAllowsExplicitTrustedLocalOptIn proves the same
// escape hatch Windows always needs also works on Linux when explicitly set
// (#4267's AC3: a real degrade with a recorded marker, not a silent no-op).
func TestConfigureNoNetworkAllowsExplicitTrustedLocalOptIn(t *testing.T) {
	t.Setenv(allowUnisolatedNetworkNoneEnv, "1")
	cmd := exec.Command("/bin/true")

	marker, err := configureNoNetwork(cmd)
	if err != nil {
		t.Fatalf("configureNoNetwork() error = %v", err)
	}
	if marker != unsupportedNetworkIsolationMarker {
		t.Fatalf("marker = %q, want %q", marker, unsupportedNetworkIsolationMarker)
	}
	if got := cmd.Env[len(cmd.Env)-1]; got != "GOOBERS_NETWORK_ISOLATION="+unsupportedNetworkIsolationMarker {
		t.Fatalf("network isolation marker (child env) = %q", got)
	}
	if cmd.SysProcAttr.Cloneflags&(syscall.CLONE_NEWUSER|syscall.CLONE_NEWNET) != 0 {
		t.Fatalf("Cloneflags = %v, want no namespace flags once isolation is skipped", cmd.SysProcAttr.Cloneflags)
	}
}

// TestNetworkNoneStartFailureHintNamesRestrictedUserNS proves #4267's
// diagnostic: a network:none stage's fork/exec EPERM is enriched with text
// naming the missing capability and both remedies.
func TestNetworkNoneStartFailureHintNamesRestrictedUserNS(t *testing.T) {
	startErr := &exec.Error{Name: "goobers", Err: syscall.EPERM}
	err := describeNetworkNoneStartFailure("none", startErr)
	if err == nil {
		t.Fatal("describeNetworkNoneStartFailure() = nil")
	}
	for _, want := range []string{
		"unprivileged user namespaces",
		"restricted",
		"sysctl",
		allowUnisolatedNetworkNoneEnv,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("error = %v, want it to still wrap the original EPERM", err)
	}
}

// TestNetworkNoneStartFailureHintLeavesOtherFailuresAlone proves only the
// EPERM shape is diagnosed: an unrelated failure (e.g. a missing binary)
// passes through with no added, misleading namespace guidance.
func TestNetworkNoneStartFailureHintLeavesOtherFailuresAlone(t *testing.T) {
	startErr := &exec.Error{Name: "goobers", Err: syscall.ENOENT}
	err := describeNetworkNoneStartFailure("none", startErr)
	if err != startErr { //nolint:errorlint // asserting the identical, unwrapped error comes back, not merely that it's still reachable via errors.Is
		t.Fatalf("describeNetworkNoneStartFailure() = %v, want the original error unchanged", err)
	}
}

// TestNetworkNoneStartFailureHintIgnoresOtherNetworkModes proves the hint
// only applies to network:none stages — a start failure for any other mode
// (or none configured) must not be rewritten.
func TestNetworkNoneStartFailureHintIgnoresOtherNetworkModes(t *testing.T) {
	startErr := &exec.Error{Name: "goobers", Err: syscall.EPERM}
	err := describeNetworkNoneStartFailure("", startErr)
	if err != startErr { //nolint:errorlint // asserting the identical, unwrapped error comes back, not merely that it's still reachable via errors.Is
		t.Fatalf("describeNetworkNoneStartFailure() = %v, want the original error unchanged for mode \"\"", err)
	}
}
