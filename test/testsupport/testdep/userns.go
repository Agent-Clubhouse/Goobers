package testdep

import (
	"context"
	"errors"
	"os"
	"runtime"
	"time"
)

// userNamespaceProbeTimeout bounds the capability probe so a wedged fork/exec
// can never hang the suite. The probe only execs a no-op binary, so this is
// orders of magnitude more headroom than it needs.
const userNamespaceProbeTimeout = 30 * time.Second

// goos is overridable in tests so the Linux-only gate below stays exercisable
// regardless of the build host. Reading it through a variable also keeps
// staticcheck from folding the comparison away as build-tag dead code.
var goos = runtime.GOOS

// userNamespaceProbe is overridable in tests. The real implementation lives in
// userns_linux.go and forks a throwaway child into a new unprivileged user
// namespace — the exact capability bubblewrap and the executor's network:none
// isolation are built on.
var userNamespaceProbe = probeUserNamespace

// RequireUserNamespaces skips the calling test, with an explicit reason naming
// the denied capability, when this environment cannot create an unprivileged
// user namespace (CLONE_NEWUSER).
//
// This is the capability half of the integration-tier gate: Require proves the
// external tool is installed, RequireUserNamespaces proves the kernel and the
// container security posture actually let it run. Bubblewrap (internal/sandbox)
// and Goobers' deterministic-stage network:none isolation
// (internal/executor/network_linux.go) both build their confinement on an
// unprivileged user namespace, so a hardened runtime — seccompProfile:
// RuntimeDefault, securityContext capabilities drop [ALL],
// allowPrivilegeEscalation false, or kernel.unprivileged_userns_clone=0 —
// denies the clone with EPERM even when the tool is installed and the kernel's
// own max_user_namespaces is generous. Without this gate that denial reads as a
// sandbox or product regression when it is really an environment capability gap
// (#3397).
//
// Only a permission-shaped failure triggers a skip. Any other probe failure (a
// missing no-op binary, a fork resource limit) is a genuine environment problem
// rather than a capability gap, so RequireUserNamespaces leaves it alone and
// lets the caller's own setup surface it normally — the same conservative rule
// test/testsupport/netns applies to the network-isolation flavour of this
// check. The comparison is against os.ErrPermission rather than syscall.EPERM
// so this file stays platform-neutral; syscall.Errno maps both EPERM and
// EACCES onto it.
//
// The probe runs entirely inside a throwaway child process, so it never changes
// the namespace membership, credentials, or threads of the test process itself.
func RequireUserNamespaces(t TB) {
	t.Helper()
	if goos != "linux" {
		t.Skipf(
			"integration test skipped: unprivileged user namespaces are a Linux "+
				"capability and %s cannot provide CLONE_NEWUSER", goos,
		)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), userNamespaceProbeTimeout)
	defer cancel()
	err := userNamespaceProbe(ctx)
	if err == nil {
		return
	}
	if !errors.Is(err, os.ErrPermission) {
		return
	}
	t.Skipf(
		"integration test skipped: unprivileged user namespaces unavailable "+
			"(CLONE_NEWUSER: operation not permitted) — the environment denies the "+
			"capability this test requires; the usual cause is the runtime's security "+
			"posture (a seccomp profile, capabilities: drop [ALL], or "+
			"kernel.unprivileged_userns_clone=0) rather than a missing tool: %v", err,
	)
}
