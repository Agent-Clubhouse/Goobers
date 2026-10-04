// Package workflowgraph provides shared workflow graph queries.
package workflowgraph

import "github.com/goobers/goobers/internal/workflow"

// BranchContainsState reports whether target is reachable from start.
func BranchContainsState(machine *workflow.Machine, start, target string) bool {
	seen := make(map[string]bool)
	stack := []string{start}
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == target {
			return true
		}
		if state == "" || workflow.IsReservedAnyTarget(state) || seen[state] || !machine.Has(state) {
			continue
		}
		seen[state] = true
		stack = append(stack, machine.Outgoing(state)...)
	}
	return false
}
