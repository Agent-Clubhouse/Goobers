package engine

import (
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/journal"
)

const gatePlacementChange = "gate-placement-provenance-v1"

// Reviewer dispatches have their own retry loop. Preserve their substrate
// observations at that boundary, including typed failure evidence. Older
// histories emitted no gate placement, even when activity results contained
// it; the marker preserves those histories' journal activity payloads.
func recordGatePlacement(ctx workflow.Context, rec *runJournal, stage string, attempt int, class journal.AttemptClass, result stageActivityResult, dispatchErr error) {
	if workflow.GetVersion(ctx, gatePlacementChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return
	}
	if dispatchErr != nil && result.Placement == nil {
		result.Placement = DispatchFailurePlacement(dispatchErr)
	}
	rec.placement(ctx, stage, attempt, class, result)
}
