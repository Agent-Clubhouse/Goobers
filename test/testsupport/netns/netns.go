// Package netns gates tests that require Goobers' enforced network
// isolation behind a real capability probe, so environments that block
// unprivileged user namespaces get a declared skip instead of a false red
// (#3397).
package netns

import (
	"context"
	"errors"
	"runtime"
	"syscall"

	"github.com/goobers/goobers/internal/executor"
)

// TB is the minimal testing surface RequireIsolation needs.
type TB interface {
	Helper()
	Skipf(format string, args ...any)
}

// probe is overridable in tests. Production callers get
// executor.ProbeNoNetwork — the same capability check Goobers' own WSL
// preflight uses (cmd/goobers/wslpreflight.go) — so a pass here is proof the
// real feature works in this environment, not a permission heuristic guess.
var probe = executor.ProbeNoNetwork

// goos is overridable in tests so the Linux-only gate below is exercisable
// regardless of the build host.
var goos = runtime.GOOS

// RequireIsolation skips the calling test, with an explicit reason naming the
// missing capability, when this environment cannot create the unprivileged
// user + network namespace (CLONE_NEWUSER|CLONE_NEWNET) that Goobers'
// deterministic-stage network:none isolation relies on on Linux
// (internal/executor/network_linux.go). Hardened runtimes (seccompProfile:
// RuntimeDefault + capabilities: drop [ALL], or
// kernel.unprivileged_userns_clone=0) deny the underlying clone/unshare with
// EPERM; without this gate that reads as a demo/product regression when it
// is really an environment capability gap.
//
// Only the EPERM shape triggers a skip. Any other probe failure (e.g. a
// missing /bin/true) is a genuine environment problem, not a capability gap,
// so RequireIsolation leaves it alone and lets the caller's own isolation
// setup surface it normally.
//
// Non-Linux platforms enforce network:none through a different mechanism
// (darwin: sandbox-exec) that this capability gap does not apply to, so the
// probe is a no-op there; callers still need their own platform gating for
// platforms Goobers doesn't enforce isolation on at all.
func RequireIsolation(ctx context.Context, t TB) {
	t.Helper()
	if goos != "linux" {
		return
	}
	err := probe(ctx)
	if err == nil {
		return
	}
	if !errors.Is(err, syscall.EPERM) {
		return
	}
	t.Skipf("network-isolation test skipped: unprivileged user namespaces "+
		"(CLONE_NEWUSER|CLONE_NEWNET) are blocked in this environment "+
		"(missing CAP_SYS_ADMIN, or kernel.unprivileged_userns_clone=0, or "+
		"a seccomp/capabilities-drop profile denies the clone): %v", err)
}
