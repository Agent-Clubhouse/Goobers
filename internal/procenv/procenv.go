// Package procenv is the single source of truth for the ambient
// daemon-process env vars carried into a stage or harness subprocess.
// internal/executor and internal/harness each spawn subprocesses (a
// deterministic stage's shell command, an agentic CLI invocation) and both
// need the identical default-deny allowlist (SEC-045) — before this package
// existed, the two copies drifted apart (#98 extended only the harness list;
// #122 caught and re-synced it by hand). Delegating both to BaseEnv keeps
// them structurally unable to drift again.
package procenv

import (
	"os"
	"strings"
)

// Vars are the exact ambient env var names carried through: PATH/HOME/TMPDIR
// (required for the child, and anything it shells out to, e.g. `make`
// invoking `go`, to locate its own toolchain); XDG_CONFIG_HOME/XDG_DATA_HOME
// (XDG base-directory tools); LANG (locale); SSL_CERT_FILE (custom CA
// bundles behind a corporate proxy); HTTP_PROXY/HTTPS_PROXY/NO_PROXY; and the
// Go toolchain family (#248) — GOPATH/GOBIN/GOCACHE/GOMODCACHE (relocated
// caches), GOFLAGS/GOPROXY/GOSUMDB/GOPRIVATE/GONOSUMCHECK (module resolution
// behind a corporate proxy or private registry), GOTOOLCHAIN (toolchain
// selection) — without which `local-ci`'s `make ci` re-downloads modules or
// fails outright on any host with a customized Go env. None of these carries
// secret material — the allowlist stays default-deny.
var Vars = []string{
	"PATH", "HOME", "TMPDIR",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"LANG",
	"SSL_CERT_FILE",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"GOPATH", "GOBIN", "GOCACHE", "GOMODCACHE",
	"GOFLAGS", "GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOSUMCHECK",
	"GOTOOLCHAIN",
}

// Prefixes are ambient env var name prefixes carried through as a family —
// LC_* (POSIX locale category overrides: LC_ALL, LC_CTYPE, LC_COLLATE, ...)
// is the only one. A var must actually start with one of these prefixes, not
// merely share a substring, to stay default-deny (e.g. "LC_TOKEN" passes;
// "LOCALE_TOKEN" does not).
var Prefixes = []string{"LC_"}

// BaseEnv returns the minimal, explicit env every stage or harness process
// starts with: Vars (exact names) plus the Prefixes families, carried
// forward from the daemon process, and nothing else — never the full
// os.Environ().
func BaseEnv() []string {
	env := make([]string, 0, len(Vars))
	for _, name := range Vars {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, prefix := range Prefixes {
			if strings.HasPrefix(name, prefix) {
				env = append(env, kv)
				break
			}
		}
	}
	return env
}
