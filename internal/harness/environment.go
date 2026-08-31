package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	capabilitypkg "github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/ephemeraltmp"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/telemetry"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type credentialEnvConfig struct {
	adapterName                    string
	envCapabilities                map[string]string
	optionalCredentialCapabilities map[string]bool
	extraEnvAllowlist              []string
	instanceRoot                   string
	selfBin                        string
	// ephemeralTmp, when non-nil, is this attempt's private temp area — the
	// self-runner binding of `tmp:ephemeral` (internal/ephemeraltmp). It is
	// applied to the ambient allowlist FIRST, before anything below adds to
	// the environment, for two reasons: nothing added below is a temp path, so
	// applying it early costs nothing; and both adapters override TMPDIR again
	// afterwards for their own in-workspace runtime confinement (Claude
	// unconditionally, Copilot under an enforced sandbox), which must keep
	// winning — that confinement is already attempt-private and rides
	// workspace cleanup, and pointing it at a directory outside the sandbox's
	// writable roots would break the stage. What the binding still delivers in
	// that case is the half the adapters never covered: the temp-nested build
	// caches (GOCACHE and friends) the agent's own `go build`/`npm`/`cargo`
	// invocations write, which is where the unbounded growth actually came
	// from.
	ephemeralTmp *ephemeraltmp.Scope
}

func baseEnv(extra []string) []string {
	return procenv.BaseEnvWith(extra)
}

// establishEphemeralTmp carves this attempt's private temp area when the
// runner enforces `tmp:ephemeral`, and returns nil when it does not. The
// caller reclaims it (defer) on every exit path.
//
// Failure is a hard stop, not a downgrade: the solver has already told the
// operator that runner `self` enforces this effect, so running the harness
// against ambient temp instead would be a confident PASS on unenforced
// substrate (docs/design/goobernetes-restrictions.md D4).
func establishEphemeralTmp(adapterName string, enabled bool, root string) (*ephemeraltmp.Scope, error) {
	if !enabled {
		return nil, nil
	}
	scope, err := ephemeraltmp.Establish(root)
	if err != nil {
		return nil, fmt.Errorf("harness: %s: bind tmp:ephemeral: %w", adapterName, err)
	}
	return scope, nil
}

func buildCredentialEnv(ctx context.Context, cfg credentialEnvConfig, req RunRequest) ([]string, error) {
	env := baseEnv(cfg.extraEnvAllowlist)
	if cfg.ephemeralTmp != nil {
		scoped, err := cfg.ephemeralTmp.Apply(env)
		if err != nil {
			return nil, fmt.Errorf("harness: %s: bind tmp:ephemeral: %w", cfg.adapterName, err)
		}
		env = scoped
	}
	telemetryDir := req.TelemetryDir
	if telemetryDir == "" {
		telemetryDir = telemetry.PrepareStageTelemetryDir(req.Workspace)
	}
	if telemetryDir != "" {
		env = append(env, telemetry.StageTelemetryEnv+"="+telemetryDir)
	}
	if cfg.instanceRoot != "" {
		env = append(env, executor.InstanceRootEnvVar+"="+cfg.instanceRoot)
	}
	if cfg.selfBin != "" {
		env = append(env, executor.GoobersBinEnvVar+"="+cfg.selfBin)
	}
	if repo := req.Envelope.RepoRef; repo.Provider != "" {
		env = append(env,
			executor.RepoProviderEnvVar+"="+string(repo.Provider),
			executor.RepoOwnerEnvVar+"="+repo.Owner,
			executor.RepoNameEnvVar+"="+repo.Name,
		)
		if repo.Project != "" {
			env = append(env, executor.RepoProjectEnvVar+"="+repo.Project)
		}
	}
	if refs := req.Envelope.AdditionalWorkspaces; len(refs) > 0 {
		type refWorkspace struct{ name, path string }
		items := make([]refWorkspace, 0, len(refs))
		for _, workspace := range refs {
			if workspace.Name == "" || workspace.Path == "" {
				continue
			}
			items = append(items, refWorkspace{workspace.Name, workspace.Path})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
		names := make([]string, 0, len(items))
		for _, item := range items {
			env = append(env, executor.AdditionalRepoEnvVar(item.name)+"="+item.path)
			names = append(names, item.name)
		}
		if len(names) > 0 {
			env = append(env, executor.AdditionalReposEnvVar+"="+strings.Join(names, ","))
		}
	}
	storedCopilotAuth := false
	modelCapability := string(capabilitypkg.AgentModel)
	if slices.Contains(req.Envelope.Capabilities, modelCapability) &&
		cfg.optionalCredentialCapabilities[modelCapability] &&
		cfg.envCapabilities[modelCapability] == "COPILOT_GITHUB_TOKEN" {
		if req.Credentials == nil {
			return nil, fmt.Errorf("harness: %s: resolve %s: no credential set", cfg.adapterName, modelCapability)
		}
		if _, err := req.Credentials.Token(ctx, modelCapability); err != nil {
			if errors.Is(err, credentials.ErrNoCredentialForCapability) {
				storedCopilotAuth = true
			} else {
				return nil, fmt.Errorf("harness: %s: resolve %s: %w", cfg.adapterName, modelCapability, err)
			}
		}
	}
	if storedCopilotAuth {
		env = withoutEnvVars(env, "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN")
	}
	for _, capability := range req.Envelope.Capabilities {
		envVar, ok := cfg.envCapabilities[capability]
		if !ok {
			continue
		}
		if req.Credentials == nil {
			return nil, fmt.Errorf("harness: %s: resolve %s: no credential set", cfg.adapterName, capability)
		}
		token, err := req.Credentials.Token(ctx, capability)
		if err != nil {
			// A missing grant is tolerated in two cases: an explicitly optional
			// capability (the CLI can fall back to an existing user session,
			// e.g. agent:model), or an Azure DevOps repo. ADO repo credentials
			// are provisioned dynamically per stage through adoauth (azure-cli/
			// workload/managed-identity shell out to `az`), NOT through the
			// static capability→token grant map, so azure-cli auth deliberately
			// configures no repo:push grant. The agent commits locally under the
			// modify-repository policy action; the deterministic push-branch
			// stage publishes the branch with an az-derived credential. A
			// PAT-configured ADO repo still has a grant and injects normally —
			// only the absence of one is tolerated, so GitHub stays fail-closed.
			if errors.Is(err, credentials.ErrNoCredentialForCapability) &&
				(cfg.optionalCredentialCapabilities[capability] ||
					req.Envelope.RepoRef.Provider == apiv1.ProviderADO) {
				continue
			}
			return nil, fmt.Errorf("harness: %s: resolve %s: %w", cfg.adapterName, capability, err)
		}
		if storedCopilotAuth && isCopilotModelFallbackEnv(envVar) {
			if capability == string(capabilitypkg.RepoPush) {
				// Shipped authoring workflows commit locally and publish through
				// a later deterministic push stage. Keep that stage's scoped
				// credential in the materialized Set, but never expose it as
				// GH_TOKEN to a Copilot subprocess using stored model auth.
				continue
			}
			return nil, fmt.Errorf(
				"harness: %s: stored Copilot login cannot be used with capability %s because it injects %s; configure a distinct agent:model credential",
				cfg.adapterName,
				capability,
				envVar,
			)
		}
		env = append(env, envVar+"="+token)
	}
	return env, nil
}

func withoutEnvVars(env []string, names ...string) []string {
	filtered := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		reserved := false
		for _, candidate := range names {
			if strings.EqualFold(name, candidate) {
				reserved = true
				break
			}
		}
		if !reserved {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func isCopilotModelFallbackEnv(name string) bool {
	return strings.EqualFold(name, "COPILOT_GITHUB_TOKEN") ||
		strings.EqualFold(name, "GH_TOKEN") ||
		strings.EqualFold(name, "GITHUB_TOKEN")
}
