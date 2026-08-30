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
	"regexp"
	"strings"
)

// Vars are the exact ambient env var names carried through: PATH/HOME/USER/TMPDIR
// (required for the child, and anything it shells out to, e.g. `make`
// invoking `go`, to locate its own toolchain and authenticated user config);
// Windows profile paths
// (USERPROFILE/APPDATA/LOCALAPPDATA/HOMEDRIVE/HOMEPATH), required by
// authenticated CLIs such as Copilot to find their existing per-user config;
// Windows runtime/shell paths (SystemRoot/WINDIR/TEMP/TMP/ComSpec/PATHEXT/
// PSModulePath), required for Node CSPRNG initialization and PowerShell tool
// execution;
// the Windows Program* install roots (#3753) —
// ProgramData/ProgramFiles/ProgramFiles(x86)/ProgramW6432/CommonProgramFiles/
// CommonProgramFiles(x86) — from which NuGet derives its machine-wide settings
// path, without which any stage shelling out to `dotnet build`, `dotnet test`
// or `msbuild` dies on "Failed to load NuGet settings";
// XDG_CONFIG_HOME/XDG_DATA_HOME
// (XDG base-directory tools); LANG (locale); SSL_CERT_FILE (custom CA
// bundles behind a corporate proxy); HTTP_PROXY/HTTPS_PROXY/NO_PROXY; the
// Go toolchain family (#248) — GOPATH/GOBIN/GOCACHE/GOMODCACHE (relocated
// caches), GOFLAGS/GOPROXY/GOSUMDB/GOPRIVATE/GONOSUMCHECK (module resolution
// behind a corporate proxy or private registry), GOTOOLCHAIN (toolchain
// selection) — without which `local-ci`'s `make ci` re-downloads modules or
// fails outright on any host with a customized Go env; and the common
// non-Go toolchain families (#736, polyglot) — .NET/NuGet, Python, Node, Rust,
// Java/Maven/Gradle, and Playwright (#3369) — so a stage running
// `dotnet build && dotnet test`, `pip`, `npm`, `cargo`, `mvn`, `gradle`, or a
// Playwright-driven E2E suite against a relocated SDK root, cache, or
// configured package registry finds it instead of silently falling back to a
// HOME-derived default that does not exist on the host.
// None of these carries secret material — the allowlist stays default-deny,
// and toolchain vars that CAN carry secrets (e.g. npm's per-registry
// `npm_config_//…/:_authToken`) are deliberately excluded, which is why the
// Node cache var is allowlisted by its exact name rather than an `npm_config_`
// prefix. A team that needs a var not listed here declares it explicitly via
// instance config (RunnerConfig.EnvPassthrough) — see BaseEnvWith — never by
// switching to os.Environ() passthrough.
var Vars = []string{
	"PATH", "HOME", "USER", "TMPDIR",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH",
	"SystemRoot", "WINDIR", "TEMP", "TMP", "ComSpec", "PATHEXT", "PSModulePath",
	// Windows Program* install roots (#3753): NuGet builds its machine-wide
	// settings path from ProgramData, so without these `dotnet build`/`dotnet
	// test`/`msbuild` fails immediately with "Failed to load NuGet settings.
	// Value cannot be null. (Parameter 'path1')" and a misleading MSB4236.
	"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
	"CommonProgramFiles", "CommonProgramFiles(x86)",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"LANG",
	"SSL_CERT_FILE",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"GOPATH", "GOBIN", "GOCACHE", "GOMODCACHE",
	"GOFLAGS", "GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOSUMCHECK",
	"GOTOOLCHAIN",
	// .NET / NuGet: SDK/runtime root, CLI home, package + http caches, and the
	// telemetry/logo behavior flags `dotnet build`/`dotnet test` honor.
	"DOTNET_ROOT", "DOTNET_CLI_HOME",
	"NUGET_PACKAGES", "NUGET_HTTP_CACHE_PATH",
	"DOTNET_CLI_TELEMETRY_OPTOUT", "DOTNET_NOLOGO",
	// Python: active virtualenv, import path, per-user base, and pip cache.
	"VIRTUAL_ENV", "PYTHONPATH", "PYTHONUSERBASE", "PIP_CACHE_DIR",
	// Node: module resolution path, the (secret-free) npm cache location, and
	// npm registry host routing for lockfiles with public registry URLs.
	"NODE_PATH", "npm_config_cache", "NPM_CONFIG_REGISTRY", "NPM_CONFIG_REPLACE_REGISTRY_HOST",
	// Rust: cargo + rustup homes (registry cache, toolchains).
	"CARGO_HOME", "RUSTUP_HOME",
	// Java: JDK roots, JVM options, and Maven + Gradle homes/caches.
	"JAVA_HOME", "JDK_HOME", "MAVEN_OPTS", "MAVEN_CONFIG", "M2_HOME",
	"GRADLE_OPTS", "GRADLE_USER_HOME",
	// Playwright: relocated browser binary cache (#3369) — same secret-free
	// cache-location class as GOCACHE/npm_config_cache/PIP_CACHE_DIR/
	// NUGET_PACKAGES/CARGO_HOME/GRADLE_USER_HOME; without it a Playwright-driven
	// stage falls back to a HOME-derived default that doesn't exist in a fresh
	// sandbox HOME and attempts a CDN download the egress policy denies.
	"PLAYWRIGHT_BROWSERS_PATH",
}

// Prefixes are ambient env var name prefixes carried through as a family —
// LC_* (POSIX locale category overrides: LC_ALL, LC_CTYPE, LC_COLLATE, ...)
// is the only one. A var must actually start with one of these prefixes, not
// merely share a substring, to stay default-deny (e.g. "LC_TOKEN" passes;
// "LOCALE_TOKEN" does not).
var Prefixes = []string{"LC_"}

// envNamePattern is the POSIX portable env var name shape
// (IEEE Std 1003.1 §8.1): a letter or underscore followed by letters, digits,
// or underscores. It rejects names carrying '=', NUL, or shell metacharacters,
// so an instance-config-declared passthrough name (BaseEnvWith) can never
// smuggle an assignment or a second var into the child env.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidName reports whether name is a well-formed environment variable name
// safe to add to the allowlist. Instance-config validation uses it to fail
// closed on a malformed RunnerConfig.EnvPassthrough entry at load time rather
// than silently dropping (or worse, mis-splitting) it at stage-launch time.
func ValidName(name string) bool {
	return envNamePattern.MatchString(name)
}

// BaseEnv returns the minimal, explicit env every stage or harness process
// starts with: Vars (exact names) plus the Prefixes families, carried
// forward from the daemon process, and nothing else — never the full
// os.Environ(). It is BaseEnvWith with no instance-declared additions.
func BaseEnv() []string {
	return BaseEnvWith(nil)
}

// BaseEnvWith returns BaseEnv extended with the instance-config-declared
// passthrough names in extra (RunnerConfig.EnvPassthrough, #736): each is
// carried through by its exact name, additively, on top of the built-in
// allowlist. This stays default-deny — extra is an explicit operator opt-in
// list of names, never os.Environ() passthrough — and is deduplicated so a
// name already covered by Vars or a Prefixes family is emitted once. A name in
// extra that is unset in the daemon environment is simply absent from the
// result, exactly like an unset built-in.
func BaseEnvWith(extra []string) []string {
	emitted := make(map[string]struct{}, len(Vars)+len(extra))
	env := make([]string, 0, len(Vars)+len(extra))
	add := func(name string) {
		if _, dup := emitted[name]; dup {
			return
		}
		emitted[name] = struct{}{}
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for _, name := range Vars {
		add(name)
	}
	for _, name := range extra {
		add(name)
	}
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, dup := emitted[name]; dup {
			continue
		}
		for _, prefix := range Prefixes {
			if strings.HasPrefix(name, prefix) {
				emitted[name] = struct{}{}
				env = append(env, kv)
				break
			}
		}
	}
	return env
}
