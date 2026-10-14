package validate

import (
	"fmt"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/mcpconfig"
)

// checkMCPCredentialProviders reports MCP002 for an external MCP server
// credentialRef that names a first-party capability the harness never
// materialises on the provider of a gaggle that runs the goober.
//
// At stage start the harness applies capability.FitsProvider to every non-byo
// reference against the run's repository, which is the gaggle's spec.project,
// and fails the stage when it does not fit, because the server declared that
// it needs the credential. A goober runs on the gaggles whose workflows name
// it from an agentic task or agentic gate; an unused goober never runs and is
// not reported, whether it is gaggle-owned or instance-shared. A kind: byo
// reference is the operator's own named credential and fits every provider.
func (ix *index) checkMCPCredentialProviders(r *Report) {
	users := ix.agenticGooberGaggles()
	names := make([]string, 0, len(ix.goobers))
	for name := range ix.goobers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		goober := ix.goobers[name]
		for _, gaggleName := range users[name] {
			gaggle, ok := ix.gaggles[gaggleName]
			if !ok {
				continue
			}
			for _, server := range goober.Spec.MCPServers {
				for _, ref := range server.CredentialRefs {
					if message, misfit := mcpCredentialProviderMisfit(server.Name, ref, gaggle); misfit {
						r.addWarning(WarningMCPCredentialProvider, ix.gooberFile[name], gaggleName, "Goober", name, "%s", message)
					}
				}
			}
		}
	}
}

// mcpCredentialProviderMisfit returns the MCP002 message for ref on server
// when its capability does not fit gaggle's project provider.
func mcpCredentialProviderMisfit(server string, ref apiv1.MCPCredentialRef, gaggle apiv1.Gaggle) (string, bool) {
	if ref.Kind == apiv1.MCPCredentialKindBYO {
		return "", false
	}
	key := mcpconfig.CredentialKey(ref)
	owner, owned := capability.OwnerProvider(key)
	project := gaggle.Spec.Project.Provider
	if !owned || capability.FitsProvider(key, project) {
		return "", false
	}
	message := fmt.Sprintf("MCP server %q credentialRef capability %q belongs to %s repositories, but gaggle %q's project is on %s, "+
		"so the credential is never materialised and every stage this goober runs there fails before the harness starts",
		server, key, owner, gaggle.Name, mcpProviderLabel(project))
	if backlog := gaggle.Spec.Backlog.Provider; backlog != "" && backlog != project && capability.FitsProvider(key, backlog) {
		message += fmt.Sprintf("; the gaggle's backlog is on %s, but an agentic stage's credentials follow the project repository, "+
			"so issue capabilities are not routed to the backlog provider for it", backlog)
	}
	return message + "; bind the server's own credential with kind: byo and a named ref (instance credentials: mcp:), or remove the reference", true
}

// agenticGooberGaggles maps each goober named by a workflow's agentic task or
// agentic gate to the sorted gaggles of those workflows: the gaggles it runs on.
func (ix *index) agenticGooberGaggles() map[string][]string {
	seen := map[string]map[string]bool{}
	note := func(goober, gaggle string) {
		if goober == "" || gaggle == "" {
			return
		}
		if seen[goober] == nil {
			seen[goober] = map[string]bool{}
		}
		seen[goober][gaggle] = true
	}
	for _, indexed := range ix.workflows {
		w := indexed.definition
		for _, task := range w.Spec.Tasks {
			if task.Type == apiv1.TaskAgentic {
				note(task.Goober, w.Spec.Gaggle)
			}
		}
		for _, gate := range w.Spec.Gates {
			if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil {
				note(gate.Agentic.Goober, w.Spec.Gaggle)
			}
		}
	}
	out := make(map[string][]string, len(seen))
	for goober, gaggles := range seen {
		for gaggle := range gaggles {
			out[goober] = append(out[goober], gaggle)
		}
		sort.Strings(out[goober])
	}
	return out
}

// mcpProviderLabel names a gaggle project's provider for MCP002, rendering
// the legacy empty provider the way the harness treats it.
func mcpProviderLabel(provider apiv1.Provider) string {
	if provider == "" {
		return "github (no provider set)"
	}
	return string(provider)
}
