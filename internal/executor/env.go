package executor

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/procenv"
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

// baseEnv returns the minimal, explicit env every stage process starts with
// — internal/procenv.BaseEnv(), the allowlist internal/harness's baseEnv()
// shares (#248, closing the #98/#122 drift for good: one definition instead
// of two hand-kept-in-sync copies). No os.Environ() passthrough (SEC-045).
func baseEnv() []string {
	return procenv.BaseEnv()
}

// buildStageEnv resolves credentials for declared, and returns the full
// process env for the stage: baseEnv(), the definition's explicit env, one
// GOOBERS_CRED_* var per declared capability that has a materialized credential,
// plus — only when injectRunContext is set — GOOBERS_RUN_ID/GOOBERS_WORKFLOW/
// GOOBERS_INSTANCE_ROOT (instanceRoot may be empty — see ShellExecutor.InstanceRoot),
// and one GOOBERS_INPUT_* var per entry in inputs. Every resolved token is also
// registered with registrar so it can be scrubbed from anything the stage's
// process writes.
//
// Inputs/RunID/WorkflowID/InstanceRoot are the only way a `goobers` CLI
// subcommand invoked as a stage's command (e.g. backlog-query/open-pr/
// issue-close-out, #131/#132) learns its declared Task.Inputs or which run
// it's part of — DeterministicRun.Command is a static argv, and
// InvocationEnvelope is otherwise an in-process value never serialized to
// the child.
//
// injectRunContext is false for a stage whose command is NOT the goobers CLI
// (e.g. local-ci's `make ci`), so the runner's operational identity does not
// leak into a stage that runs the project's own build/test suite (#322): a
// self-hosting project's local-ci runs `go test ./...`, and any test that
// reads a GOOBERS_* var would otherwise be silently perturbed by whatever the
// live run set. Only goobers-CLI stages, which genuinely consume run context,
// receive it — the least-privilege env boundary. The GOOBERS_INPUT_* vars are
// unaffected: a stage's own declared inputs are its config, not the runner's
// operational identity, so they flow to every stage kind regardless.
//
// A declared capability with no configured grant is silently skipped
// (credentials.Injector's own contract — not every capability is
// credentialed); resolution failure for a capability that IS granted fails
// closed.
func buildStageEnv(ctx context.Context, injector *credentials.Injector, declared []string, registrar credentials.SecretRegistrar, runID, workflowID, instanceRoot string, injectRunContext bool, inputs map[string]interface{}, declaredEnv map[string]string) ([]string, error) {
	env := baseEnv()
	keys := make([]string, 0, len(declaredEnv))
	for key := range declaredEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := declaredEnv[key]
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, errors.New("executor: declared environment contains an invalid name or value")
		}
		env = append(env, key+"="+value)
	}
	// GOTRACEBACK=all makes every Go stage subprocess (go test under `make ci`,
	// the goobers CLI, goober-runtime) print ALL goroutines — including runtime
	// and system stacks — when it dumps on SIGQUIT (the timeout-diagnostics path
	// in shell.go) or its own -test.timeout. No runtime/perf cost: it only
	// changes what a crash/quit dump contains. Set here so a hung stage's
	// captured artifact shows the complete blocked-goroutine picture, not just
	// user goroutines.
	env = append(env, "GOTRACEBACK=all")
	if injectRunContext {
		env = append(env, "GOOBERS_RUN_ID="+runID, "GOOBERS_WORKFLOW="+workflowID)
		if instanceRoot != "" {
			env = append(env, "GOOBERS_INSTANCE_ROOT="+instanceRoot)
		}
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
