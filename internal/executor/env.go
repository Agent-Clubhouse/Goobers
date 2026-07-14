package executor

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/credentials"
)

// CredentialEnvVar returns the deterministic env var name a stage's declared
// capability is injected under, e.g. "github:issues:write" ->
// "GOOBERS_CRED_GITHUB_ISSUES_WRITE". Exported so a `goobers` CLI subcommand
// invoked as a stage's shell command (e.g. backlog-query/open-pr/
// issue-close-out, #131/#132) can look up its own injected credential by the
// same convention buildStageEnv uses to set it, without duplicating the
// sanitization rule.
func CredentialEnvVar(capability string) string {
	sanitized := nonAlnum.ReplaceAllString(capability, "_")
	return "GOOBERS_CRED_" + strings.ToUpper(sanitized)
}

// InputEnvVar returns the deterministic env var name a stage's declared
// Task.Inputs key is passed through under, e.g. "trustLabel" ->
// "GOOBERS_INPUT_TRUSTLABEL". Exported for the same reason as
// CredentialEnvVar above.
func InputEnvVar(key string) string {
	sanitized := nonAlnum.ReplaceAllString(key, "_")
	return "GOOBERS_INPUT_" + strings.ToUpper(sanitized)
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

// buildStageEnv resolves credentials for declared, and returns the full
// process env for the stage: baseEnv() plus one GOOBERS_CRED_* var per
// declared capability that has a materialized credential, plus
// GOOBERS_RUN_ID/GOOBERS_WORKFLOW/GOOBERS_INSTANCE_ROOT (instanceRoot may be
// empty — see ShellExecutor.InstanceRoot) and one GOOBERS_INPUT_* var per
// entry in inputs. Every resolved token is also registered with registrar so
// it can be scrubbed from anything the stage's process writes.
//
// Inputs/RunID/WorkflowID/InstanceRoot are the only way a `goobers` CLI
// subcommand invoked as a stage's command (e.g. backlog-query/open-pr/
// issue-close-out, #131/#132) learns its declared Task.Inputs or which run
// it's part of — DeterministicRun.Command is a static argv, and
// InvocationEnvelope is otherwise an in-process value never serialized to
// the child.
//
// A declared capability with no configured grant is silently skipped
// (credentials.Injector's own contract — not every capability is
// credentialed); resolution failure for a capability that IS granted fails
// closed.
func buildStageEnv(ctx context.Context, injector *credentials.Injector, declared []string, registrar credentials.SecretRegistrar, runID, workflowID, instanceRoot string, inputs map[string]interface{}) ([]string, error) {
	env := baseEnv()
	env = append(env, "GOOBERS_RUN_ID="+runID, "GOOBERS_WORKFLOW="+workflowID)
	if instanceRoot != "" {
		env = append(env, "GOOBERS_INSTANCE_ROOT="+instanceRoot)
	}
	for k, v := range inputs {
		if s, ok := v.(string); ok {
			env = append(env, InputEnvVar(k)+"="+s)
		}
	}
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
		env = append(env, CredentialEnvVar(capability)+"="+token)
	}
	return env, nil
}
