package executor

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/internal/telemetry"
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

const (
	// InstanceRootEnvVar carries the instance root to goobers CLI stages.
	InstanceRootEnvVar = "GOOBERS_INSTANCE_ROOT"

	// GoobersBinEnvVar carries the running daemon's executable path to agentic
	// harnesses that need to invoke a goobers CLI subcommand.
	GoobersBinEnvVar = "GOOBERS_BIN"

	// BranchNamespaceEnvVar is the env var a goobers-CLI stage reads to learn its
	// gaggle's configured run-branch namespace root (GaggleSpec.BranchNamespace,
	// providers.DefaultBranchNamespace when unset). PR-selector defaults
	// (headPrefix) and run-branch head derivation resolve it via this var so a
	// gaggle that retunes its namespace selects, opens, and remediates PRs under
	// the same prefix its branches use and the mirror-fetch exclusion preserves
	// (#965/#1010). Injected only under injectRunContext, alongside GOOBERS_GAGGLE.
	BranchNamespaceEnvVar = "GOOBERS_BRANCH_NAMESPACE"

	// BaseBranchEnvVar is the env var a goobers-CLI stage reads to learn its
	// gaggle's configured default branch (GaggleSpec.Project.Branch/RepoRef.
	// Branch, "main" when unset) — the branch every worktree is actually
	// forked from. PR-lifecycle stages (pr-select, open-pr, rebase-pr, ...)
	// resolve their "base" input default via this var instead of assuming
	// "main" (#2087). Injected only under injectRunContext, alongside
	// GOOBERS_GAGGLE.
	BaseBranchEnvVar = "GOOBERS_BASE_BRANCH"

	// BuiltinErrorFileEnvVar carries an executor-owned file path through which
	// goobers CLI stages report typed failures even when the stage declares no
	// resultFile. It is an internal subprocess protocol, not a DSL input.
	BuiltinErrorFileEnvVar = "GOOBERS_BUILTIN_ERROR_FILE"

	// TriggerRefEnvVar carries the bounded reference for the trigger that
	// started the run. Only goobers CLI stages receive it.
	TriggerRefEnvVar = "GOOBERS_TRIGGER_REF"

	// RepoProviderEnvVar carries the scheduler-routed repository provider to
	// goobers CLI stages.
	RepoProviderEnvVar = "GOOBERS_REPO_PROVIDER"
	// RepoOwnerEnvVar carries the scheduler-routed repository owner to goobers
	// CLI stages.
	RepoOwnerEnvVar = "GOOBERS_REPO_OWNER"
	// RepoNameEnvVar carries the scheduler-routed repository name to goobers
	// CLI stages.
	RepoNameEnvVar = "GOOBERS_REPO_NAME"
	// RepoProjectEnvVar carries the scheduler-routed repository project to
	// goobers CLI stages. It is empty for GitHub (which has no project tier)
	// and set to the Azure DevOps project for ADO-routed repositories, which
	// need organization/project/repo to address a repo.
	RepoProjectEnvVar = "GOOBERS_REPO_PROJECT"

	// AdditionalReposEnvVar carries the comma-separated names of the gaggle's
	// read-only reference-repo checkouts (MGV-11 #1286) available to this stage,
	// so a stage can discover which GOOBERS_ADDITIONAL_REPO_* vars are set.
	AdditionalReposEnvVar = "GOOBERS_ADDITIONAL_REPOS"
)

// AdditionalRepoEnvVar returns the deterministic env var name a read-only
// reference-repo checkout's path is injected under, e.g. a repo named "goobers"
// -> "GOOBERS_ADDITIONAL_REPO_GOOBERS" (MGV-11 #1286). Same sanitization rule as
// CredentialEnvVar so a stage can reconstruct the name from the repo name.
func AdditionalRepoEnvVar(name string) string {
	sanitized := nonAlnum.ReplaceAllString(name, "_")
	return "GOOBERS_ADDITIONAL_REPO_" + strings.ToUpper(sanitized)
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// baseEnv returns the minimal, explicit env every stage process starts with
// — internal/procenv.BaseEnvWith, the allowlist internal/harness's baseEnv()
// shares (#248, closing the #98/#122 drift for good: one definition instead
// of two hand-kept-in-sync copies). No os.Environ() passthrough (SEC-045).
// extra carries the instance-config-declared passthrough names
// (RunnerConfig.EnvPassthrough, #736), additively and still default-deny.
func baseEnv(extra []string) []string {
	return procenv.BaseEnvWith(extra)
}

// buildStageEnv resolves credentials for declared, and returns the full
// process env for the stage: baseEnv(), the definition's explicit env, one
// GOOBERS_CRED_* var per declared capability that has a materialized credential,
// plus — only when injectRunContext is set — GOOBERS_RUN_ID/GOOBERS_GAGGLE/
// GOOBERS_WORKFLOW/GOOBERS_BRANCH_NAMESPACE/GOOBERS_BASE_BRANCH/GOOBERS_INSTANCE_ROOT and the
// provider snapshot identifier associated with the scheduler evaluation (when
// present), one GOOBERS_ADDITIONAL_REPO_* path per provisioned reference repo
// when contents:read is declared, plus one GOOBERS_INPUT_* var per entry in
// inputs. ShellExecutor appends its executor-owned GOOBERS_BUILTIN_ERROR_FILE
// after this function returns.
// Every resolved token is also registered with registrar so it can be scrubbed
// from anything the stage's process writes.
//
// Inputs/RunID/Gaggle/WorkflowID/InstanceRoot are the only way a `goobers` CLI
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
func buildStageEnv(ctx context.Context, injector *credentials.Injector, declared []string, registrar credentials.SecretRegistrar, runID, gaggle, workflowID, branchNamespace, baseBranch, instanceRoot string, injectRunContext bool, inputs map[string]interface{}, declaredEnv map[string]string, extraEnvAllowlist []string, additionalRepos map[string]string) ([]string, error) {
	env := baseEnv(extraEnvAllowlist)
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
	// the goobers CLI) print ALL goroutines — including runtime
	// and system stacks — when it dumps on SIGQUIT (the timeout-diagnostics path
	// in shell.go) or its own -test.timeout. No runtime/perf cost: it only
	// changes what a crash/quit dump contains. Set here so a hung stage's
	// captured artifact shows the complete blocked-goroutine picture, not just
	// user goroutines.
	// Left unconditional intentionally (#2172): a non-Go stage (`dotnet test`,
	// `npm run ci`, `pytest`) never reads this var, so it is silently inert for
	// those stacks rather than harmful — no gating on a declared go-family
	// capability needed. See the identical call-out already carried in
	// config-examples/gaggles/dotnet-service/workflows/dotnet-implementation.yaml
	// (AC5, #1093).
	env = append(env, "GOTRACEBACK=all")
	if injectRunContext {
		env = append(env, "GOOBERS_RUN_ID="+runID, "GOOBERS_GAGGLE="+gaggle, "GOOBERS_WORKFLOW="+workflowID)
		if branchNamespace != "" {
			env = append(env, BranchNamespaceEnvVar+"="+branchNamespace)
		}
		if baseBranch != "" {
			env = append(env, BaseBranchEnvVar+"="+baseBranch)
		}
		if instanceRoot != "" {
			env = append(env, InstanceRootEnvVar+"="+instanceRoot)
		}
		if snapshotID := providersnapshot.ID(ctx); snapshotID != "" {
			env = append(env, providersnapshot.EnvVar+"="+snapshotID)
		}
	}
	if slices.Contains(declared, string(capability.ContentsRead)) && len(additionalRepos) > 0 {
		names := make([]string, 0, len(additionalRepos))
		for name := range additionalRepos {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			env = append(env, AdditionalRepoEnvVar(name)+"="+additionalRepos[name])
		}
		env = append(env, AdditionalReposEnvVar+"="+strings.Join(names, ","))
	}
	for k, v := range inputs {
		if s, ok := v.(string); ok {
			env = append(env, InputEnvVar(k)+"="+s)
		}
	}
	if injector == nil || len(declared) == 0 {
		return env, nil
	}
	// A credential that cannot be materialized is an infrastructure fault, not
	// evidence about the work (#3361): typed with its own code AND marked via
	// the invoke.InfrastructureFailure seam, so the runner retries it on the
	// bounded infrastructure budget (journal AttemptClass "infra") instead of
	// consuming the stage's policy attempts — at attempt budgets of 1, the old
	// classification converted a transient 403 into a terminal work failure.
	set, err := injector.Materialize(ctx, declared)
	if err != nil {
		return nil, invoke.InfrastructureFailure(StageFailure(telemetry.ErrCodeCredentialUnavailable, err))
	}
	for _, capability := range declared {
		token, err := set.Token(ctx, capability)
		if err != nil {
			if errors.Is(err, credentials.ErrNoCredentialForCapability) {
				continue // declared but uncredentialed capability (e.g. telemetry:read)
			}
			// Same seam as Materialize above: a granted capability whose token
			// resolution fails at env-build time is credential infrastructure.
			return nil, invoke.InfrastructureFailure(StageFailure(telemetry.ErrCodeCredentialUnavailable, err))
		}
		registrar.Register([]byte(token))
		env = append(env, CredentialEnvVar(capability)+"="+token)
	}
	return env, nil
}
