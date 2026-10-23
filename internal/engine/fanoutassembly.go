package engine

import (
	"fmt"

	"go.temporal.io/sdk/workflow"

	wf "github.com/goobers/goobers/internal/workflow"
)

const fanOutAssemblyVersion = "engine-run-fanout-assembly-v1"

// refuseUnassembledFanOut preserves the run-start refusal introduced before
// production assembly. Admission rejects parallels independently, so older
// test-only histories that walked to unknown-state were never admitted runs.
// The versioned path delegates maps to the unchanged run-start refusal; plain
// and selective parallels proceed through compilation and preview/live checks.
func refuseUnassembledFanOut(ctx workflow.Context, in RunInput) error {
	spec := in.Spec
	if len(spec.Parallels) > 0 && workflow.GetVersion(ctx, fanOutAssemblyVersion, workflow.DefaultVersion, 1) != workflow.DefaultVersion {
		spec.Parallels = nil
	}
	return wf.RefuseUnsupportedFanOut(spec)
}

// initializeFanOut attaches the shared controller to the normal Run lifecycle
// before its first journal operation. Static parallels and the selection/map
// adapters share this commit owner and run-wide budget. Admission remains the
// authority for enabled features; this does not lift admission refusals.
func (r *runJournal) initializeFanOut(ctx workflow.Context, in RunInput) error {
	if len(in.Spec.Parallels) == 0 && len(in.Spec.Maps) == 0 {
		return nil
	}
	if !in.previewFeaturesEnabled() || !in.LiveJournal {
		return fmt.Errorf("engine: fan-out requires preview features and live journaling")
	}
	if r.owner == nil {
		r.owner = &journalCommitOwner{root: r, mu: workflow.NewMutex(ctx)}
	}
	r.staticWalk = &staticWalkSession{seen: map[string]bool{}}
	r.stepBudget = &walkStepBudget{}
	return nil
}
