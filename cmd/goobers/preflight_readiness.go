package main

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/credreadiness"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/runtimeplan"
	"github.com/goobers/goobers/internal/workflow"
)

type runtimePreflightHarness struct {
	Stage   string              `json:"stage"`
	Harness string              `json:"harness"`
	Process runtimeplan.Process `json:"process"`
	Source  runtimeplan.Source  `json:"source"`
	harness.Readiness
}

// Projection only: do not call buildGaggleCredentials or construct a resolver,
// since GitHub CLI resolution and dynamic credential sources can read secrets.
func runtimeCredentialSources(ctx context.Context, cfg *instance.Config, gaggle apiv1.GaggleSpec) ([]credentials.Grant, map[string]credreadiness.Check, error) {
	sources := make(map[string]credreadiness.Check)
	var bindings []credentials.RepoBinding
	for _, repo := range cfg.Repos {
		owner := repo.Owner
		if repo.Provider == "ado" && repo.Project != "" {
			owner += "/" + repo.Project
		}
		name := owner + "/" + repo.Name
		ref := ""
		if repo.Token.Configured() || repo.GitHubAppAuth() || adoRepositoryMintsCredential(repo) {
			ref = name
			sources[ref] = credreadiness.Presence(ctx, ref, repo.Token.CredentialTokenRef(ref), nil)
			if repo.GitHubAppAuth() {
				sources[ref] = runtimeDynamicCredential(credreadiness.SourceGitHubApp, ref)
			}
			if adoRepositoryMintsCredential(repo) && !repo.Token.Configured() {
				sources[ref] = runtimeDynamicCredential(credreadiness.SourceUnsupported, ref)
			}
		}
		bindings = append(bindings, credentials.RepoBinding{Owner: owner, Name: repo.Name, TokenRef: ref})
	}
	role := gaggleBacklogRole(gaggle.Project, gaggle.Backlog)
	var overrides []credentials.Grant
	if cfg.DaemonIdentity != nil {
		overrides = append(overrides, daemonIdentityOverrides(role)...)
		if cfg.DaemonIdentity.Token != nil {
			sources[daemonIdentityRefName] = credreadiness.Presence(ctx, daemonIdentityRefName, cfg.DaemonIdentity.Token.CredentialTokenRef(daemonIdentityRefName), nil)
		} else {
			sources[daemonIdentityRefName] = runtimeDynamicCredential(credreadiness.SourceGitHubApp, daemonIdentityRefName)
		}
	}
	for _, grant := range cfg.Credentials {
		key, err := credentialGrantStorageKey(grant)
		if err != nil {
			return nil, nil, err
		}
		ref := credentialRefName(key)
		overrides = append(overrides, credentials.Grant{Capability: key, Ref: ref})
		sources[ref] = credreadiness.Presence(ctx, key, grant.Token.CredentialTokenRef(ref), nil)
		if grant.GitHubApp != nil {
			sources[ref] = runtimeDynamicCredential(credreadiness.SourceGitHubApp, grant.GitHubApp.Name)
		}
	}
	owner := gaggle.Project.Owner
	if gaggle.Project.Provider == apiv1.ProviderADO && gaggle.Project.Project != "" {
		owner += "/" + gaggle.Project.Project
	}
	grants := withoutNonADORepoGrants(cfg.Repos, credentials.RunnerGrants(bindings, owner, gaggle.Project.Name, role, repoCredentialedCapabilityNames(), overrides))
	var additional []credentials.RepoBinding
	for _, repo := range gaggle.AdditionalRepos {
		owner := repo.Owner
		if repo.Provider == apiv1.ProviderADO && repo.Project != "" {
			owner += "/" + repo.Project
		}
		additional = append(additional, credentials.RepoBinding{Owner: owner, Name: repo.Name})
	}
	grants = append(grants, credentials.AdditionalReadGrants(referenceReadBindings(cfg.Repos, bindings), additional, string(capability.ContentsRead))...)
	return grants, sources, nil
}

func runtimeDynamicCredential(kind credreadiness.SourceKind, name string) credreadiness.Check {
	status := credreadiness.StatusUnobservable
	if kind == credreadiness.SourceUnsupported {
		status = credreadiness.StatusUnsupportedSource
	}
	return credreadiness.Check{Kind: kind, Source: name, Status: status, Detail: "dynamic credential availability requires secret resolution or token minting; not performed"}
}

func runtimePreflightReadiness(ctx context.Context, cfg *instance.Config, gaggle apiv1.GaggleSpec, goobers map[string]apiv1.GooberSpec, stages []runtimePreflightStage, process runtimeplan.Process, probe bool) ([]runtimeplan.CredentialCheck, []runtimePreflightHarness, error) {
	grants, sources, err := runtimeCredentialSources(ctx, cfg, gaggle)
	if err != nil {
		return nil, nil, err
	}
	var credentialChecks []runtimeplan.CredentialCheck
	var harnessChecks []runtimePreflightHarness
	for _, stage := range stages {
		keys := append([]string(nil), stage.CredentialCapabilities...)
		for _, key := range mcpconfig.BYOCredentialKeys(goobers[stage.Goober].MCPServers) {
			if !containsString(keys, key) {
				keys = append(keys, key)
			}
		}
		if stage.Harness != "" && !containsString(keys, "agent:model") {
			keys = append(keys, "agent:model")
		}
		stageGrants := grants
		if stage.Goober != "" {
			stageGrants = buildGooberCredentialGrants(stage.Goober, stage.Harness, keys, grants)
		}
		byCapability := make(map[string]string)
		for _, grant := range stageGrants {
			byCapability[grant.Capability] = grant.Ref
		}
		for _, capability := range keys {
			check, ok := sources[byCapability[capability]]
			if !ok {
				check = credreadiness.Check{Kind: credreadiness.SourceUnsupported, Status: credreadiness.StatusUnobservable, Detail: "no configured credential grant for this capability; credential requirement unverified"}
				if mcpconfig.IsBYOCredentialKey(capability) {
					check.Status = credreadiness.StatusAbsent
					check.Detail = "required named MCP credential grant is absent"
				}
			}
			check.Name = capability
			credentialChecks = append(credentialChecks, runtimeplan.CredentialObservation(stage.Name, check, process))
		}
		if stage.Harness != "" {
			harnessChecks = append(harnessChecks, runtimeHarnessReadiness(ctx, cfg, stage, goobers[stage.Goober], process, probe))
		}
	}
	return credentialChecks, harnessChecks, nil
}

var runtimeReadinessAdapterFor = adapterFor

func runtimeHarnessReadiness(ctx context.Context, cfg *instance.Config, stage runtimePreflightStage, spec apiv1.GooberSpec, process runtimeplan.Process, probe bool) runtimePreflightHarness {
	result := runtimePreflightHarness{Stage: stage.Name, Harness: stage.Harness, Process: process,
		Source: runtimeplan.Source{Fidelity: "unobservable", Detail: "harness subprocess probes disabled; use --check-readiness; target daemon/worker remains unobservable"}}
	adapter, err := runtimeReadinessAdapterFor(apiv1.Harness(stage.Harness), harnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand, nil)
	if err != nil {
		result.Checks = []harness.ReadinessCheck{{Category: "unavailable_tool", Code: "harness_adapter_unsupported", Outcome: "unsupported", Detail: "configured harness adapter unavailable; diagnostic values omitted"}}
		return result
	}
	if !probe {
		result.Checks = []harness.ReadinessCheck{harness.CheckReadinessConfig(adapter, spec), {Category: "unobservable", Code: "harness_probe_unobservable", Outcome: "unobservable", Detail: "bounded read-only probes not requested"}}
		return result
	}
	result.Source = runtimeplan.Source{Fidelity: "observed", Detail: "bounded read-only reporting-process probes; no target identity attestation"}
	grant, _ := agentModelGrant(cfg, apiv1.Harness(stage.Harness))
	result.Readiness = harness.ProbeReadiness(ctx, adapter, spec, grant != nil)
	return result
}

// Preserve invalid harness configuration as a report finding. Execution still
// uses strict admission. Only the typed harness-model/options failure gets this
// diagnostic path; all workflow structural checks and digest code are shared.
func compileRuntimePreflight(configDir string, set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec, instructions map[string]string, cfg *instance.Config) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[localscheduler.WorkflowIdentity]string, map[string]apiv1.GooberSpec, error) {
	machines, digests, resolved, _, err := compiledMachinesWithGooberDigestsAndWarnings(configDir, set, goobers, instructions, harnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand, true, nil, cfg.ExternalTelemetryConnectorNames())
	var mismatch *gooberHarnessConfigError
	if !errors.As(err, &mismatch) {
		return machines, digests, resolved, err
	}
	registry, err := buildHarnessRegistry(nil, harnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand, "", "", true, nil, false)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, spec := range goobers {
		h := spec.Harness
		if h == "" {
			h = apiv1.HarnessCopilot
		}
		if err := mcpconfig.ValidateForHarness(h, spec.MCPServers, spec.Capabilities, spec.Tools); err != nil {
			return nil, nil, nil, errors.New("invalid MCP configuration; diagnostic values omitted")
		}
	}
	machines, err = compileWorkflowMachines(set, goobers, registry.Names(), cfg.ExternalTelemetryConnectorNames())
	if err != nil {
		return nil, nil, nil, err
	}
	digests, err = computeMachineGooberDigests(machines, goobers, instructions, func(gaggle string, specs map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
		return loadGooberSkillPackages(configDir, gaggle, specs)
	})
	return machines, digests, goobers, err
}
