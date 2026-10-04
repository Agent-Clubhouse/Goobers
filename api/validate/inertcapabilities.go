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

// inertADORebinding names, for each ado:* capability that no built-in DSL 2.0
// stage consumes, what authorizes the operation instead under the rebinding
// rule (docs/design/ado-parity-dsl-2-0.md §3.1): a github:* capability
// declared on a provider-dispatched stage authorizes the same operation on
// the provider the stage routes to and selects its credential. The advice is
// provider-neutral because the warning fires on every provider. Two names are
// absent on purpose (§3.3): ado:pr:complete is honoured when declared, and
// ado:pr:status is accepted on report-pr-status but harmless (nothing reads
// it). ado:work-items:write is consumed only by open-pr, so its advice says
// that renaming it would not keep open-pr's native work-item link.
// ado:packaging:read has no built-in DSL 2.0 consumer; custom stages may keep
// it when they read Azure Artifacts feeds directly.
var inertADORebinding = map[capability.Capability]string{
	capability.ADOCodeRead:  "no capability is needed: repository reads use the repository credential",
	capability.ADOPRComment: fmt.Sprintf("%q authorizes this operation on the stage's provider", capability.GitHubPRWrite),
	capability.ADOPRWrite:   fmt.Sprintf("%q authorizes this operation on the stage's provider", capability.GitHubPRWrite),
	capability.ADOWorkItemsWrite: fmt.Sprintf("%q authorizes work-item updates on the backlog provider (%q for reads); only open-pr consumes %q, to link a pull request to its work item",
		capability.GitHubIssuesWrite, capability.GitHubIssuesRead, capability.ADOWorkItemsWrite),
	capability.ADOPackagingRead: "no built-in DSL 2.0 stage consumes it; keep it only when the stage's own command reads Azure Artifacts package feeds",
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
// declaration is not reported, and a task that runs its own command gets
// wording of its own (inertTaskCapabilityMessage). DSL 3.0 is left to the
// provider access layer design (docs/design/provider-access-layer.md).
func (ix *index) checkInertADOCapabilities(r *Report) {
	gooberUsers := map[string]bool{}
	for _, indexed := range ix.workflows {
		w := indexed.definition
		if w.DSLVersion != supportmatrix.V2DSLVersion {
			continue
		}
		for _, task := range w.Spec.Tasks {
			for _, value := range task.Capabilities {
				if message, inert := inertTaskCapabilityMessage(task, value); inert {
					r.addWarning(WarningInertADOCapability, indexed.file, w.Spec.Gaggle, "Workflow", w.Name, "%s", message)
				}
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

// inertTaskCapabilityMessage returns the CAP006 message for an inert ado:*
// capability value that task declares, and false when there is nothing to
// report. A built-in `goobers` stage that does not consume it gets the
// rebinding advice. A task that runs its own command (a custom deterministic
// command or an agentic stage) receives the credential its declared
// capabilities select, so the name may be in use there: that warning says so
// instead of telling the author to rename it.
func inertTaskCapabilityMessage(task apiv1.Task, value string) (string, bool) {
	advice, inert := inertADOCapabilityAdvice(value)
	if !inert {
		return "", false
	}
	entry, builtin := taskBuiltinStage(task)
	if !builtin {
		return fmt.Sprintf("task %q declares capability %q, which no built-in DSL 2.0 stage consumes; the task's own command receives the credential it selects, so keep it only if that command uses it (for built-in stages, %s; rebinding rule, docs/design/ado-parity-dsl-2-0.md §3.1)",
			task.Name, value, advice), true
	}
	if slices.ContainsFunc(entry.Capabilities, func(use providerstage.CapabilityUse) bool {
		return string(use.Capability) == value
	}) {
		return "", false
	}
	return fmt.Sprintf("task %q declares capability %q, which no DSL 2.0 stage consumes on this task; %s (rebinding rule, docs/design/ado-parity-dsl-2-0.md §3.1)",
		task.Name, value, advice), true
}

// taskBuiltinStage returns the provider-stage manifest row of the built-in
// `goobers` subcommand task runs, and false when it runs anything else.
func taskBuiltinStage(task apiv1.Task) (providerstage.Command, bool) {
	if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" {
		return providerstage.Command{}, false
	}
	return providerstage.Lookup(task.Run.Command[1])
}
