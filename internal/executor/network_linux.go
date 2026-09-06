//go:build linux

package executor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// allowUnisolatedNetworkNoneEnv lets network:none proceed without enforced
// isolation on a Linux host that restricts unprivileged user namespaces
// (#4267) — the same opt-in Windows always needs (network_windows.go),
// reused here so there is exactly one env var to document regardless of OS.
const allowUnisolatedNetworkNoneEnv = "GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE"

// unsupportedNetworkIsolationMarker mirrors network_windows.go's marker
// (#2034): a non-empty value means this network:none stage did NOT actually
// run isolated, so ShellExecutor.Run must journal it in the stage's own
// Outputs, not only this process's own env.
const unsupportedNetworkIsolationMarker = "unsupported-linux-userns"

func configureNoNetwork(cmd *exec.Cmd) (marker string, err error) {
	uid := os.Getuid()
	gid := os.Getgid()
	// Every live stage arrives here via proc.Configure first, which already
	// allocates SysProcAttr — but a caller that skips that (e.g.
	// ProbeNoNetwork's bare exec.Cmd, used by the WSL preflight and the
	// #3397 test capability probe) would otherwise nil-pointer-dereference
	// on the very next line. Match proc.Configure's own idiom so this
	// function is safe to call directly.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if os.Getenv(allowUnisolatedNetworkNoneEnv) == "1" {
		// Explicit, narrowly-scoped opt-out (#4267): the operator has
		// already been told by `goobers preflight` or the demo's own
		// warning that unprivileged user namespaces are restricted here.
		// Skip the clone flags below entirely rather than let them fail.
		cmd.Env = append(cmd.Env, "GOOBERS_NETWORK_ISOLATION="+unsupportedNetworkIsolationMarker)
		return unsupportedNetworkIsolationMarker, nil
	}
	// The one-ID user namespace lets a non-root daemon create the network
	// namespace without granting it any capability in the host namespaces.
	cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{
		ContainerID: uid,
		HostID:      uid,
		Size:        1,
	}}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{
		ContainerID: gid,
		HostID:      gid,
		Size:        1,
	}}
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	// Real isolation, always applied when the escape hatch above isn't set —
	// no unsupported-isolation marker in that case (#2034 is Windows-only by
	// default; Linux only needs one when the env var opt-out fires).
	return "", nil
}

// networkNoneStartFailureHint tells describeNetworkNoneStartFailure
// (network.go) what to add to a network:none stage's fork/exec failure. Only
// the EPERM shape is diagnosed (#4267): everything else (a missing binary,
// an unrelated permission problem, ...) passes through unchanged — the same
// EPERM-only gate test/testsupport/netns.RequireIsolation already uses to
// tell a real capability gap from any other failure.
func networkNoneStartFailureHint(err error) string {
	if !errors.Is(err, syscall.EPERM) {
		return ""
	}
	return "unprivileged user namespaces (CLONE_NEWUSER|CLONE_NEWNET) appear to be " +
		"restricted on this host, needed for network:none isolation; enable them " +
		"(e.g. `sysctl -w kernel.unprivileged_userns_clone=1`, or the container/" +
		"security-profile equivalent — `goobers preflight` reports this), or set " +
		allowUnisolatedNetworkNoneEnv + "=1 to run this stage without enforced isolation"
}
