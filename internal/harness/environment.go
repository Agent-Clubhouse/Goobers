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
}

func baseEnv(extra []string) []string {
	return procenv.BaseEnvWith(extra)
}

func buildCredentialEnv(ctx context.Context, cfg credentialEnvConfig, req RunRequest) ([]string, error) {
	env := baseEnv(cfg.extraEnvAllowlist)
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
	for _, capability := range req.Envelope.Capabilities {
		envVar, ok := cfg.envCapabilities[capability]
		if !ok {
			continue
		}
		if storedCopilotAuth && envVar == "GH_TOKEN" {
			if capability == string(capabilitypkg.RepoPush) {
				// Shipped authoring workflows commit locally and publish through
				// a later deterministic push stage. Keep that stage's scoped
				// credential in the materialized Set, but never expose it as
				// GH_TOKEN to a Copilot subprocess using stored model auth.
				continue
			}
			return nil, fmt.Errorf(
				"harness: %s: stored Copilot login cannot be used with capability %s because it injects GH_TOKEN; configure a distinct agent:model credential",
				cfg.adapterName,
				capability,
			)
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
		env = append(env, envVar+"="+token)
	}
	return env, nil
}
