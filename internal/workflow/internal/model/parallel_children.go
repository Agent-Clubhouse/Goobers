package model

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// ParallelHasChildStage selects blocks that require isolated writable branch
// forks. A child-capable join or unrelated stage does not opt in the branches.
// Admission still validates the DSL version, policy and execution backend.
func (m *Machine) ParallelHasChildStage(parallel apiv1.Parallel) bool {
	seen := map[string]bool{}
	var queue []string
	for _, branch := range parallel.Branches {
		queue = append(queue, branch.Start)
	}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		if state == "" || state == parallel.Join || IsReservedAnyTarget(state) || seen[state] {
			continue
		}
		seen[state] = true
		if task, ok := m.Task(state); ok && task.ChildWorkflows != nil {
			return true
		}
		queue = append(queue, m.Outgoing(state)...)
	}
	return false
}
