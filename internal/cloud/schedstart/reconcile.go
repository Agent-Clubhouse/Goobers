package schedstart

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/localscheduler"
)

// OpenRun is one run still holding a scheduler slot.
type OpenRun struct {
	Identity  localscheduler.WorkflowIdentity
	RunID     string
	Suspended bool
}

// OpenRuns enumerates the instance's open runs (claims and Temporal), the cloud
// counterpart of scanning run journals.
type OpenRuns func(context.Context) ([]OpenRun, error)

type reconciler struct{ open OpenRuns }

// NewReconciler returns the scheduler's startup inventory over open cloud runs.
// A successor leader rebuilds admission from it before the first Tick, so the
// predecessor's in-flight runs keep counting against concurrency instead of
// being admitted over a second time. The scheduler passes filesystem roots; the
// cloud has none, so they are ignored. Enumeration failure is returned: an
// empty inventory would silently reset every counter.
func NewReconciler(open OpenRuns) localscheduler.RunDirReconciler { return reconciler{open: open} }

func (r reconciler) FromRoots(ctx context.Context, _ []string) (localscheduler.RunInventory, error) {
	return r.inventory(ctx)
}

func (r reconciler) FromRunDirs(ctx context.Context, _ []string) (localscheduler.RunInventory, error) {
	return r.inventory(ctx)
}

func (r reconciler) inventory(ctx context.Context) (localscheduler.RunInventory, error) {
	if r.open == nil {
		return localscheduler.RunInventory{}, errors.New("schedstart: open-run source is required")
	}
	runs, err := r.open(ctx)
	if err != nil {
		return localscheduler.RunInventory{}, err
	}
	inventory := localscheduler.RunInventory{
		Active: map[localscheduler.WorkflowIdentity]int{},
		Runs:   map[string]localscheduler.ReconciledRun{},
	}
	for _, run := range runs {
		if run.RunID == "" {
			return localscheduler.RunInventory{}, errors.New("schedstart: open run without an id")
		}
		if _, dup := inventory.Runs[run.RunID]; dup {
			continue
		}
		if !run.Suspended { // a run parked on a child holds no slot, as locally
			inventory.Active[run.Identity]++
		}
		inventory.Runs[run.RunID] = localscheduler.ReconciledRun{
			Identity: run.Identity, Workflow: run.Identity.Workflow, Suspended: run.Suspended,
		}
	}
	return inventory, nil
}
