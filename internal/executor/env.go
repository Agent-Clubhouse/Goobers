package executor

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/credentials"
)

// TokenSource resolves a capability-scoped credential. *credentials.Set
// (internal/credentials, #14) satisfies this structurally — this package
// depends only on the method shape, not the concrete type, so a caller can
// supply any pre-materialized credential source (or a fake, in tests)
// without this package needing to know how it was built.
type TokenSource interface {
	Token(ctx context.Context, capability string) (string, error)
}

// credentialEnvVar returns the deterministic env var name a stage's declared
// capability is injected under, e.g. "github:issues:write" ->
// "GOOBERS_CRED_GITHUB_ISSUES_WRITE".
func credentialEnvVar(capability string) string {
	sanitized := nonAlnum.ReplaceAllString(capability, "_")
	return "GOOBERS_CRED_" + strings.ToUpper(sanitized)
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// passthroughVars are the only ambient daemon-process env vars carried into a
// stage's process — never the full os.Environ(). Each is required for the
// child (and the subprocesses it may itself exec, e.g. `make` invoking `go`)
// to locate its own toolchain; none carries secret material.
var passthroughVars = []string{"PATH", "HOME", "TMPDIR"}

// baseEnv returns the minimal, explicit env every stage process starts with:
// a handful of non-secret toolchain vars carried forward from the daemon
// process, and nothing else. No os.Environ() passthrough (SEC-045).
func baseEnv() []string {
	env := make([]string, 0, len(passthroughVars))
	for _, name := range passthroughVars {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// buildStageEnv resolves a token for each declared capability via tokens, and
// returns the full process env for the stage: baseEnv() plus one
// GOOBERS_CRED_* var per declared capability that has a credential. Every
// resolved token is also registered with registrar so it can be scrubbed from
// anything the stage's process writes.
//
// A declared capability with no configured grant is silently skipped (not
// every capability is credentialed, e.g. "telemetry:read"); resolution
// failure for a capability that IS granted fails closed.
func buildStageEnv(ctx context.Context, tokens TokenSource, declared []string, registrar credentials.SecretRegistrar) ([]string, error) {
	env := baseEnv()
	if tokens == nil || len(declared) == 0 {
		return env, nil
	}
	for _, capability := range declared {
		token, err := tokens.Token(ctx, capability)
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
