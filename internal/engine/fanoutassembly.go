package engine

import (
	"fmt"

	"go.temporal.io/sdk/workflow"
)

const fanOutAssemblyVersion = "engine-run-fanout-assembly-v1"

// initializeFanOut attaches the shared controller to the normal Run lifecycle
// before its first journal operation. Static parallels and the selection/map
// adapters share this commit owner and run-wide budget. Admission remains the
// authority for which adapters are enabled; this does not lift any refusal.
func (r *runJournal) initializeFanOut(ctx workflow.Context, in RunInput) error {
	if len(in.Spec.Parallels) == 0 && len(in.Spec.Maps) == 0 {
		return nil
	}
	if workflow.GetVersion(ctx, fanOutAssemblyVersion, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return nil
	}
	if !in.previewFeaturesEnabled() || !in.LiveJournal {
		return fmt.Errorf("engine: fan-out requires preview features and live journaling")
	}
	r.owner = &journalCommitOwner{root: r, mu: workflow.NewMutex(ctx)}
	r.staticWalk = &staticWalkSession{seen: map[string]bool{}}
	r.stepBudget = &walkStepBudget{}
	return nil
}
