package schedentries

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Deps are the cloud owners the schedule and signal intake binds.
type Deps struct {
	// Queue is the PG acceptance store. Its source-cursor CAS and source receipts
	// are what make a schedule occurrence accepted exactly once across lease
	// takeovers.
	Queue triggerqueue.SchedulerPolicyStore
	// Generations pins the retained generation a start is admitted against.
	Generations childworkflow.GenerationSource
	// Build prepares the archived entry of one pinned target (the start-input
	// resolver); the pin is released when the prepared value is.
	Build func(context.Context, startintent.Target) (startintent.Prepared, error)
}

// NewSources returns the scheduler's source-start intake over the cloud
// acceptance store. Acquire pins the generation in the object-store archive for
// as long as an accepted start needs it.
func NewSources(deps Deps) (*startintent.Sources, error) {
	if deps.Queue == nil || deps.Generations == nil || deps.Build == nil {
		return nil, errors.New("schedentries: queue, generation source and build are required")
	}
	return &startintent.Sources{
		Queue: deps.Queue,
		Build: deps.Build,
		Acquire: func(ctx context.Context, target startintent.Target) (func(), error) {
			generation, err := deps.Generations.Acquire(ctx, target.ConfigGeneration)
			if err != nil {
				return nil, err
			}
			return func() { _ = generation.Release() }, nil
		},
	}, nil
}
