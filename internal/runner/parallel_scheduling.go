package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

// A child wait releases its execution slot while its branch remains unfinished.
// Even a one-slot parallel therefore needs the dispatcher to run queued siblings.
// Select by the branch bodies: a child-capable join or unrelated stage must not
// change legacy serial workspace behavior. Start, resume and rerun share this
// decision; admission and workspace validation remain separate prerequisites.
func parallelUsesDispatcher(machine *workflow.Machine, parallel apiv1.Parallel) bool {
	if parallel.MaxConcurrentBranches > 1 {
		return true
	}
	seen := map[string]bool{}
	var queue []string
	for _, branch := range parallel.Branches {
		queue = append(queue, branch.Start)
	}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		if state == "" || state == parallel.Join || workflow.IsReservedAnyTarget(state) || seen[state] {
			continue
		}
		seen[state] = true
		if task, ok := machine.Task(state); ok && task.ChildWorkflows != nil {
			return true
		}
		queue = append(queue, machine.Outgoing(state)...)
	}
	return false
}
