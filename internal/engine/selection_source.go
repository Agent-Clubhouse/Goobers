package engine

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
)

// ActFreezeParallelSelection is the activity that freezes one journal-bound
// selection source before the shared fan-out controller starts any branch.
const ActFreezeParallelSelection = "FreezeParallelSelection"

// FreezeParallelSelectionInput binds a selective parallel to the exact
// producer attempt and source artifact selected by deterministic history.
type FreezeParallelSelectionInput struct {
	RunID        string                  `json:"runId"`
	ParallelName string                  `json:"parallelName"`
	Definition   apiv1.Parallel          `json:"definition"`
	Source       journal.FanOutSourceRef `json:"source"`
}

// FreezeParallelSelectionResult is recorded directly in Temporal history.
// The controller consumes Selection without consulting the source again.
type FreezeParallelSelectionResult struct {
	ParallelName string                    `json:"parallelName"`
	Selection    journal.ParallelSelection `json:"selection"`
}

// FreezeParallelSelection reads and verifies the exact source artifact and
// returns its normalized, history-frozen selection.
func (a *Activities) FreezeParallelSelection(
	ctx context.Context,
	in FreezeParallelSelectionInput,
) (FreezeParallelSelectionResult, error) {
	if in.RunID == "" || in.ParallelName == "" || in.Definition.Name != in.ParallelName {
		return FreezeParallelSelectionResult{}, invalidFanOutActivity(
			fmt.Errorf("run ID and parallel name are required, and definition name must match parallel name"),
		)
	}
	if a.FanOutBlobs == nil {
		return FreezeParallelSelectionResult{}, classifySeamError(ErrNotConfigured)
	}
	sourceData, err := a.FanOutBlobs.Get(ctx, in.Source.Digest)
	if err != nil {
		return FreezeParallelSelectionResult{}, fmt.Errorf(
			"read parallel %q selection source %s: %w", in.ParallelName, in.Source.Digest, err,
		)
	}
	selection, err := fanout.FreezeSelection(in.Definition, in.Source, sourceData)
	if err != nil {
		return FreezeParallelSelectionResult{}, invalidFanOutActivity(err)
	}
	return FreezeParallelSelectionResult{
		ParallelName: in.ParallelName,
		Selection:    selection,
	}, nil
}
