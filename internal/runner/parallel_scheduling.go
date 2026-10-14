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
//
// A map visit (whose expanded spec carries the map's name) always runs through
// the dispatcher, at every maxConcurrent: its item branch events carry roster
// identities, and the dispatcher is the one executor that journals them (§6.3).
func parallelUsesDispatcher(machine *workflow.Machine, parallel apiv1.Parallel) bool {
	if _, isMap := machine.Map(parallel.Name); isMap {
		return true
	}
	return parallel.MaxConcurrentBranches > 1 || parallelHasChildStage(machine, parallel)
}

func parallelHasChildStage(machine *workflow.Machine, parallel apiv1.Parallel) bool {
	return machine.ParallelHasChildStage(parallel)
}
