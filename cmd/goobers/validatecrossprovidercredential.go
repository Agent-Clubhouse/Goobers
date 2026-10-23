package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/internal/runnersolve"
)

// appendCrossProviderCredentialOverrideWarnings warns (CFG012) about an
// explicit credentials: entry for a pull-request or repository capability
// while a gaggle is topology (b): backlog on GitHub or Gitea, code on Azure
// DevOps (docs/design/ado-parity-dsl-2-0.md §7.2).
//
// buildRoleCredentials appends every credentials: entry as an instance-wide
// override, after the role routing, so such an entry replaces the Azure
// DevOps repository's credential for that capability and PR and push stages
// present its token to Azure DevOps. That is right for an Azure DevOps token
// and wrong for a GitHub one, and nothing in the entry says which it is — so
// the entry is honoured and warned about rather than refused (PO ruling
// 2026-09-27). Backlog-family entries are not warned: they are the
// capabilities topology (b) routes away from Azure DevOps.
func appendCrossProviderCredentialOverrideWarnings(
	instanceFile string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	add func(code validate.WarningCode, kind, name, file, path, message string),
) {
	if cfg == nil || set == nil || len(cfg.Credentials) == 0 {
		return
	}
	gaggles := crossProviderBacklogGaggles(set)
	if len(gaggles) == 0 {
		return
	}
	for i, grant := range cfg.Credentials {
		if !isProjectFamilyCapability(capability.Capability(grant.Capability)) {
			continue
		}
		add(validate.WarningCrossProviderCredentialOverride, "Instance", "credentials", instanceFile,
			fmt.Sprintf("/credentials/%d/capability", i),
			fmt.Sprintf("credentials[%d] sources %s from its own token, which replaces the Azure DevOps repository's "+
				"credential for gaggle(s) %s, whose backlog is on another provider; stages that declare %s present "+
				"this token to Azure DevOps. Use an Azure DevOps credential here, or remove the entry so the "+
				"repository's own credential backs %s",
				i, grant.Capability, strings.Join(gaggles, ", "), grant.Capability, grant.Capability))
	}
}

// crossProviderBacklogGaggles names, sorted, the gaggles whose backlog lives
// on GitHub or Gitea for Azure DevOps code (providerconfig.CrossProviderBacklog).
func crossProviderBacklogGaggles(set *instance.ConfigSet) []string {
	var names []string
	for _, gaggle := range set.Gaggles {
		if providerconfig.CrossProviderBacklog(gaggle.Spec.Project.Provider, gaggle.Spec.Backlog.Provider) {
			names = append(names, gaggle.Name)
		}
	}
	sort.Strings(names)
	return names
}

// isProjectFamilyCapability reports whether c is a credentialed capability
// the project repository's credential backs: every credentialed capability
// outside the backlog family.
func isProjectFamilyCapability(c capability.Capability) bool {
	if isBacklogRoleCapability(c) {
		return false
	}
	for _, credentialed := range runtimewiring.CredentialedCapabilities {
		if c == credentialed {
			return true
		}
	}
	return false
}

// appendCrossProviderBacklogUnboundWarnings warns (CFG015) about a topology
// (b) gaggle whose deterministic stages declare backlog-family capabilities
// (github:issues:*, github:milestones:write) that nothing backs: the backlog
// repository has no repos[] entry with a token or GitHub App, and no
// daemonIdentity or credentials: entry sources them. Each such capability gets
// no credential at run time and the stage that declares it fails closed
// (credentials.RunnerGrants never falls back to the Azure DevOps repository's
// credential), so the gap is reported here instead of at the first run.
func appendCrossProviderBacklogUnboundWarnings(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	add func(code validate.WarningCode, kind, name, file, path, message string),
) {
	if cfg == nil || set == nil {
		return
	}
	for _, gaggle := range set.Gaggles {
		spec := gaggle.Spec
		if !providerconfig.CrossProviderBacklog(spec.Project.Provider, spec.Backlog.Provider) {
			continue
		}
		granted, err := runtimewiring.ConfiguredCredentialGrants(cfg, spec.Project, spec.Backlog)
		if err != nil {
			continue
		}
		var missing []string
		for _, c := range declaredBacklogCapabilities(set, gaggle.Name) {
			if !granted[c] {
				missing = append(missing, c)
			}
		}
		if len(missing) == 0 {
			continue
		}
		add(validate.WarningCrossProviderBacklogUnbound, "Gaggle", gaggle.Name,
			gaggleDiagnosticFile(root, configDir, set, gaggle.Name), "/spec/backlog/project",
			fmt.Sprintf("backlog repository %s (%s) has no instance repos[] entry with a token or GitHub App, and its stages "+
				"declare %s, which in topology (b) only the backlog repository's credential, a daemonIdentity or a "+
				"credentials: entry can back; those stages fail for want of a credential. Add a repos[] entry for %s "+
				"(docs/guides/ado-limitations.md)",
				spec.Backlog.Project, spec.Backlog.Provider, strings.Join(missing, ", "), spec.Backlog.Project))
	}
}

// declaredBacklogCapabilities lists, sorted and once each, the backlog-family
// capabilities the deterministic tasks of the gaggle's workflows declare.
// Agentic stages are left out: their credentials follow the Azure DevOps
// project, so a backlog repository credential never reaches them.
func declaredBacklogCapabilities(set *instance.ConfigSet, gaggle string) []string {
	seen := map[string]bool{}
	for _, workflow := range set.Workflows {
		if workflow.Spec.Gaggle != gaggle {
			continue
		}
		for _, task := range workflow.Spec.Tasks {
			for _, c := range task.Capabilities {
				if task.Type == apiv1.TaskDeterministic && isBacklogRoleCapability(capability.Capability(c)) {
					seen[c] = true
				}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for c := range seen {
		names = append(names, c)
	}
	sort.Strings(names)
	return names
}

// appendCrossProviderBacklogPlacementFinding warns (RNR007) about a task of a
// topology (b) gaggle whose eligible runner set excludes the daemon's own host.
// The engine refuses every task of such a run a stage pod
// (engine.CrossProviderBacklogPodCode), because a pod has no instance config to
// route backlog work and credentials by role, so a placement like this
// validates and then fails every run at dispatch. An unsatisfiable stage is
// left to RNR001. Under placement.selfExecution: deny no runner can run the
// task, so the finding is an error.
func appendCrossProviderBacklogPlacementFinding(
	gaggle apiv1.GaggleSpec, placement runnersolve.StagePlacement, selfRunnerNames map[string]bool,
	workflowName, file, path string,
	add func(code validate.WarningCode, severity validate.Severity, kind, name, file, path, message string),
	selfDenied bool,
) {
	if !providerconfig.CrossProviderBacklog(gaggle.Project.Provider, gaggle.Backlog.Provider) ||
		placement.Unsat != nil || placementIncludesSelf(placement.Eligible, selfRunnerNames) {
		return
	}
	severity := validate.Warning
	remedy := "place it on a self runner: declare what its runsOn requires on the self runner entry, if that host provides it, or relax its runsOn"
	if selfDenied {
		severity = validate.Error
		remedy = "placement.selfExecution: deny leaves no runner that can run it; a topology (b) gaggle's stages run only on the daemon's own host"
	}
	add(validate.RunnerCrossProviderBacklogOffSelf, severity, "Workflow", workflowName, file, path, fmt.Sprintf(
		"stage %q cannot resolve to the daemon's own host (self): its eligible runner set is %v. Its gaggle keeps the backlog on %s "+
			"and the code on %s (topology (b)), which stage pods do not support, so it will be refused at dispatch (code %q); %s",
		placement.Stage, placement.Eligible, gaggle.Backlog.Provider, gaggle.Project.Provider, engine.CrossProviderBacklogPodCode, remedy))
}
