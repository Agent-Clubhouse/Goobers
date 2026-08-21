//go:build linux

package executor

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestConfigureNoNetworkAllocatesSysProcAttr guards against a real regression
// found while building the #3397 capability probe: on the live stage path
// proc.Configure always allocates cmd.SysProcAttr before
// configureCommandNetwork runs, but ProbeNoNetwork (used by the WSL
// preflight, cmd/goobers/wslpreflight.go, and by the #3397 test capability
// probe, test/testsupport/netns) builds a bare *exec.Cmd and calls
// configureNoNetwork directly. Without this guard, that nil SysProcAttr
// panics instead of failing (or succeeding) — verified by running the built
// test binary in a hardened Linux container (cap-drop=ALL,
// no-new-privileges): before the fix, a nil-pointer-dereference panic; after,
// a clean EPERM error the caller can classify.
func TestConfigureNoNetworkAllocatesSysProcAttr(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if cmd.SysProcAttr != nil {
		t.Fatal("precondition: exec.Command must start with a nil SysProcAttr")
	}

	marker, err := configureNoNetwork(cmd)
	if err != nil {
		t.Fatalf("configureNoNetwork() error = %v, want nil (allocation only, no syscall yet)", err)
	}
	if marker != "" {
		t.Fatalf("marker = %q, want empty — linux always applies real isolation", marker)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("configureNoNetwork left cmd.SysProcAttr nil")
	}
	if got, want := cmd.SysProcAttr.Cloneflags, uintptr(syscall.CLONE_NEWUSER|syscall.CLONE_NEWNET); got != want {
		t.Fatalf("Cloneflags = %#x, want %#x", got, want)
	}
}

// TestConfigureNoNetworkPreservesCallerSysProcAttr asserts the same guard is
// idempotent-safe: a caller that (like the real stage path, via
// proc.Configure) already populated SysProcAttr keeps those fields — the nil
// check must never clobber an existing struct.
func TestConfigureNoNetworkPreservesCallerSysProcAttr(t *testing.T) {
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if _, err := configureNoNetwork(cmd); err != nil {
		t.Fatalf("configureNoNetwork() error = %v", err)
	}
	if !cmd.SysProcAttr.Setsid {
		t.Fatal("configureNoNetwork clobbered a caller-set SysProcAttr field (Setsid)")
	}
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWUSER == 0 {
		t.Fatal("configureNoNetwork did not layer its Cloneflags onto the existing SysProcAttr")
	}
}
