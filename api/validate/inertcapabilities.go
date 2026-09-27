package validate

import (
	"fmt"
	"slices"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/providerstage"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// inertADORebinding names, for each ado:* capability that no DSL 2.0 stage
// consumes, what authorizes the operation instead under the rebinding rule
// (docs/design/ado-parity-dsl-2-0.md §3.1): a github:* capability declared on
// a provider-dispatched stage authorizes the same operation on Azure DevOps
// and selects its credential. ado:pr:status and ado:pr:complete are absent on
// purpose: both are optional but honoured when declared (§3.3).
var inertADORebinding = map[capability.Capability]string{
	capability.ADOCodeRead:       "no capability is needed: repository reads use the repository credential",
	capability.ADOPRComment:      fmt.Sprintf("%q authorizes this operation on Azure DevOps", capability.GitHubPRWrite),
	capability.ADOPRWrite:        fmt.Sprintf("%q authorizes this operation on Azure DevOps", capability.GitHubPRWrite),
	capability.ADOWorkItemsWrite: fmt.Sprintf("%q authorizes this operation on Azure DevOps (%q for reads)", capability.GitHubIssuesWrite, capability.GitHubIssuesRead),
}

// inertADOCapabilityAdvice returns the rebinding advice for value when it is
// an inert ado:* name, and false otherwise.
func inertADOCapabilityAdvice(value string) (string, bool) {
	advice, ok := inertADORebinding[capability.Capability(value)]
	return advice, ok
}

// checkInertADOCapabilities reports CAP006 for every inert ado:* capability a
// DSL 2.0 workflow's tasks declare, and for every one held by a goober that a
// DSL 2.0 task or agentic gate runs. The names stay valid (configurations
// that followed older docs keep loading); the warning only says the grant has
// no consumer and names what authorizes the operation. A built-in command
// whose manifest row lists the capability does consume it (open-pr links an
// ADO pull request to its work item with ado:work-items:write), so such a
// declaration is not reported. DSL 3.0 is left to the provider access layer
// design (docs/design/provider-access-layer.md).
func (ix *index) checkInertADOCapabilities(r *Report) {
	gooberUsers := map[string]bool{}
	for _, indexed := range ix.workflows {
		w := indexed.definition
		if w.DSLVersion != supportmatrix.V2DSLVersion {
			continue
		}
		for _, task := range w.Spec.Tasks {
			for _, value := range task.Capabilities {
				advice, inert := inertADOCapabilityAdvice(value)
				if !inert || taskCommandConsumes(task, value) {
					continue
				}
				r.addWarning(WarningInertADOCapability, indexed.file, w.Spec.Gaggle, "Workflow", w.Name,
					"task %q declares capability %q, which no DSL 2.0 stage consumes on this task; %s (rebinding rule, docs/design/ado-parity-dsl-2-0.md §3.1)",
					task.Name, value, advice)
			}
			if task.Type == apiv1.TaskAgentic && task.Goober != "" {
				gooberUsers[task.Goober] = true
			}
		}
		for _, gate := range w.Spec.Gates {
			if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil && gate.Agentic.Goober != "" {
				gooberUsers[gate.Agentic.Goober] = true
			}
		}
	}
	names := make([]string, 0, len(gooberUsers))
	for name := range gooberUsers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		goober, ok := ix.goobers[name]
		if !ok {
			continue
		}
		for _, value := range goober.Spec.Capabilities {
			if advice, inert := inertADOCapabilityAdvice(value); inert {
				r.addWarning(WarningInertADOCapability, ix.gooberFile[name], goober.Spec.Gaggle, "Goober", name,
					"spec.capabilities grants %q, which no DSL 2.0 stage consumes; %s (rebinding rule, docs/design/ado-parity-dsl-2-0.md §3.1)",
					value, advice)
			}
		}
	}
}

// taskCommandConsumes reports whether task runs a built-in `goobers`
// subcommand whose provider-stage manifest row lists value.
func taskCommandConsumes(task apiv1.Task, value string) bool {
	if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" {
		return false
	}
	entry, ok := providerstage.Lookup(task.Run.Command[1])
	if !ok {
		return false
	}
	return slices.ContainsFunc(entry.Capabilities, func(use providerstage.CapabilityUse) bool {
		return string(use.Capability) == value
	})
}
