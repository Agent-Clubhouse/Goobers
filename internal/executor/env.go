package executor

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/credentials"
)

// credentialEnvVar returns the deterministic env var name a stage's declared
// capability is injected under, e.g. "github:issues:write" ->
// "GOOBERS_CRED_GITHUB_ISSUES_WRITE".
func credentialEnvVar(capability string) string {
	sanitized := nonAlnum.ReplaceAllString(capability, "_")
	return "GOOBERS_CRED_" + strings.ToUpper(sanitized)
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// passthroughVars are the only ambient daemon-process env vars carried into a
// stage's process — never the full os.Environ(). PATH/HOME/TMPDIR are
// required for the child (and the subprocesses it may itself exec, e.g.
// `make` invoking `go`) to locate its own toolchain; the rest (#122, parity
// with internal/harness's identical #75/#98 allowlist) are well-known,
// non-secret environment conventions a deterministic stage's own command
// (e.g. `make ci` behind a corporate proxy) may depend on: XDG_CONFIG_HOME/
// XDG_DATA_HOME, LANG (locale), SSL_CERT_FILE (custom CA bundles), and the
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY trio. None of these carries secret
// material — the allowlist stays default-deny (SEC-045).
var passthroughVars = []string{
	"PATH", "HOME", "TMPDIR",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"LANG",
	"SSL_CERT_FILE",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
}

// passthroughPrefixes are ambient env var name prefixes carried through as a
// family — LC_* (POSIX locale category overrides) — rather than one name at
// a time. A var must actually start with one of these prefixes, not merely
// share a substring, to stay default-deny.
var passthroughPrefixes = []string{"LC_"}

// baseEnv returns the minimal, explicit env every stage process starts with:
// the passthrough allowlist (exact names, plus the LC_* family) carried
// forward from the daemon process, and nothing else. No os.Environ()
// passthrough (SEC-045).
func baseEnv() []string {
	env := make([]string, 0, len(passthroughVars))
	for _, name := range passthroughVars {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, prefix := range passthroughPrefixes {
			if strings.HasPrefix(name, prefix) {
				env = append(env, kv)
				break
			}
		}
	}
	return env
}

// buildStageEnv resolves credentials for declared, and returns the full
// process env for the stage: baseEnv() plus one GOOBERS_CRED_* var per
// declared capability that has a materialized credential. Every resolved
// token is also registered with registrar so it can be scrubbed from
// anything the stage's process writes.
//
// A declared capability with no configured grant is silently skipped
// (credentials.Injector's own contract — not every capability is
// credentialed); resolution failure for a capability that IS granted fails
// closed.
func buildStageEnv(ctx context.Context, injector *credentials.Injector, declared []string, registrar credentials.SecretRegistrar) ([]string, error) {
	env := baseEnv()
	if injector == nil || len(declared) == 0 {
		return env, nil
	}
	set, err := injector.Materialize(ctx, declared)
	if err != nil {
		return nil, err
	}
	for _, capability := range declared {
		token, err := set.Token(ctx, capability)
		if err != nil {
			if errors.Is(err, credentials.ErrNoCredentialForCapability) {
				continue // declared but uncredentialed capability (e.g. telemetry:read)
			}
			return nil, err
		}
		registrar.Register([]byte(token))
		env = append(env, credentialEnvVar(capability)+"="+token)
	}
	return env, nil
}
