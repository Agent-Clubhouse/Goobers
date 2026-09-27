package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
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
// on GitHub or Gitea for Azure DevOps code (crossProviderBacklog).
func crossProviderBacklogGaggles(set *instance.ConfigSet) []string {
	var names []string
	for _, gaggle := range set.Gaggles {
		if crossProviderBacklog(gaggle.Spec.Project.Provider, gaggle.Spec.Backlog.Provider) {
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
	for _, credentialed := range credentialedCapabilities {
		if c == credentialed {
			return true
		}
	}
	return false
}
